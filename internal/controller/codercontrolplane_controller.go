// Package controller contains Kubernetes controllers for coder-k8s resources.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
)

const (
	defaultCoderImage         = "ghcr.io/coder/coder:latest"
	defaultControlPlanePort   = int32(80)
	controlPlaneTargetPort    = int32(8080)
	controlPlaneTLSTargetPort = int32(8443)

	postgresConnectionURLEnvVar = "CODER_PG_CONNECTION_URL"

	defaultOperatorAccessUsername = "coder-k8s-operator"
	defaultOperatorAccessEmail    = "coder-k8s-operator@coder-k8s.invalid"
	// #nosec G101 -- this is a static token label used as a database identifier.
	defaultOperatorAccessTokenName     = "coder-k8s-operator"
	defaultOperatorAccessTokenLifetime = 365 * 24 * time.Hour

	operatorAccessRetryInterval = 30 * time.Second
	operatorTokenSecretSuffix   = "-operator-token"

	workspaceRBACFinalizer          = "coder.com/workspace-rbac-cleanup"
	workspaceRBACOwnerUIDAnnotation = "coder.com/workspace-rbac-owner-uid"

	// #nosec G101 -- this is a field index key, not a credential.
	licenseSecretNameFieldIndex = ".spec.licenseSecretRef.name"

	licenseConditionReasonApplied       = "Applied"
	licenseConditionReasonPending       = "Pending"
	licenseConditionReasonSecretMissing = "SecretMissing"
	licenseConditionReasonForbidden     = "Forbidden"
	licenseConditionReasonNotSupported  = "NotSupported"
	licenseConditionReasonError         = "Error"

	workspaceRBACDriftRequeueInterval = 2 * time.Minute
	gatewayExposureRequeueInterval    = 2 * time.Minute
	licenseUploadRequestTimeout       = 30 * time.Second
	entitlementsStatusRefreshInterval = 2 * time.Minute
)

var (
	errSecretValueMissing = errors.New("secret value missing")
	errSecretValueEmpty   = errors.New("secret value empty")
)

// LicenseUploader uploads and inspects Coder Enterprise licenses in a coderd instance.
type LicenseUploader interface {
	AddLicense(ctx context.Context, coderURL, sessionToken, licenseJWT string) error
	HasAnyLicense(ctx context.Context, coderURL, sessionToken string) (bool, error)
}

// EntitlementsInspector inspects coderd entitlements.
type EntitlementsInspector interface {
	Entitlements(ctx context.Context, coderURL, sessionToken string) (codersdk.Entitlements, error)
}

// NewSDKEntitlementsInspector returns an EntitlementsInspector backed by codersdk.
func NewSDKEntitlementsInspector() EntitlementsInspector {
	return &sdkEntitlementsInspector{}
}

type sdkEntitlementsInspector struct{}

func (i *sdkEntitlementsInspector) Entitlements(ctx context.Context, coderURL, sessionToken string) (codersdk.Entitlements, error) {
	sdkClient, err := newSDKLicenseClient(coderURL, sessionToken)
	if err != nil {
		return codersdk.Entitlements{}, err
	}

	entitlements, err := sdkClient.Entitlements(ctx)
	if err != nil {
		return codersdk.Entitlements{}, fmt.Errorf("query coder entitlements: %w", err)
	}
	if entitlements.Features == nil {
		return codersdk.Entitlements{}, fmt.Errorf("assertion failed: entitlements features must not be nil")
	}

	return entitlements, nil
}

// NewSDKLicenseUploader returns a LicenseUploader backed by codersdk.
func NewSDKLicenseUploader() LicenseUploader {
	return &sdkLicenseUploader{}
}

type sdkLicenseUploader struct{}

func (u *sdkLicenseUploader) AddLicense(ctx context.Context, coderURL, sessionToken, licenseJWT string) error {
	if licenseJWT == "" {
		return fmt.Errorf("assertion failed: license JWT must not be empty")
	}

	sdkClient, err := newSDKLicenseClient(coderURL, sessionToken)
	if err != nil {
		return err
	}

	if _, err := sdkClient.AddLicense(ctx, codersdk.AddLicenseRequest{License: licenseJWT}); err != nil {
		return fmt.Errorf("upload coder license: %w", err)
	}

	return nil
}

func (u *sdkLicenseUploader) HasAnyLicense(ctx context.Context, coderURL, sessionToken string) (bool, error) {
	sdkClient, err := newSDKLicenseClient(coderURL, sessionToken)
	if err != nil {
		return false, err
	}

	licenses, err := sdkClient.Licenses(ctx)
	if err != nil {
		return false, fmt.Errorf("list coder licenses: %w", err)
	}

	return len(licenses) > 0, nil
}

func newSDKLicenseClient(coderURL, sessionToken string) (*codersdk.Client, error) {
	if strings.TrimSpace(coderURL) == "" {
		return nil, fmt.Errorf("assertion failed: coder URL must not be empty")
	}
	if sessionToken == "" {
		return nil, fmt.Errorf("assertion failed: session token must not be empty")
	}

	parsedURL, err := url.Parse(coderURL)
	if err != nil {
		return nil, fmt.Errorf("parse coder URL: %w", err)
	}

	sdkClient := codersdk.New(parsedURL)
	sdkClient.SetSessionToken(sessionToken)
	if sdkClient.HTTPClient == nil {
		sdkClient.HTTPClient = &http.Client{}
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("assertion failed: http.DefaultTransport is not *http.Transport")
	}
	// Use a dedicated transport to avoid sharing http.DefaultTransport's
	// connection pool across parallel test servers.
	sdkClient.HTTPClient.Transport = defaultTransport.Clone()
	sdkClient.HTTPClient.Timeout = licenseUploadRequestTimeout

	return sdkClient, nil
}

// CoderControlPlaneReconciler reconciles a CoderControlPlane object.
type CoderControlPlaneReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme

	OperatorAccessProvisioner coderbootstrap.OperatorAccessProvisioner
	LicenseUploader           LicenseUploader
	EntitlementsInspector     EntitlementsInspector
}

// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete

// Reconcile converges the desired CoderControlPlane spec into Deployment and Service resources.
func (r *CoderControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: reconciler client must not be nil")
	}
	if r.Scheme == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: reconciler scheme must not be nil")
	}

	coderControlPlane := &coderv1alpha1.CoderControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, coderControlPlane); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get codercontrolplane %s: %w", req.NamespacedName, err)
	}

	if coderControlPlane.Name != req.Name || coderControlPlane.Namespace != req.Namespace {
		return ctrl.Result{}, fmt.Errorf("assertion failed: fetched object %s/%s does not match request %s/%s",
			coderControlPlane.Namespace, coderControlPlane.Name, req.Namespace, req.Name)
	}

	if !coderControlPlane.DeletionTimestamp.IsZero() {
		return r.finalizeWorkspaceRBAC(ctx, coderControlPlane)
	}

	if err := r.ensureWorkspaceRBACFinalizer(ctx, req.NamespacedName, coderControlPlane); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileServiceAccount(ctx, coderControlPlane); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileWorkspaceRBAC(ctx, coderControlPlane); err != nil {
		return ctrl.Result{}, err
	}

	deployment, err := r.reconcileDeployment(ctx, coderControlPlane)
	if err != nil {
		return ctrl.Result{}, err
	}
	service, err := r.reconcileService(ctx, coderControlPlane)
	if err != nil {
		return ctrl.Result{}, err
	}
	gatewayExposureNeedsRequeue, err := r.reconcileExposure(ctx, coderControlPlane)
	if err != nil {
		return ctrl.Result{}, err
	}

	originalStatus := *coderControlPlane.Status.DeepCopy()
	nextStatus := r.desiredStatus(coderControlPlane, deployment, service)

	operatorResult, err := r.reconcileOperatorAccess(ctx, coderControlPlane, &nextStatus)
	if err != nil {
		return ctrl.Result{}, err
	}

	licenseResult, err := r.reconcileLicense(ctx, coderControlPlane, &nextStatus)
	if err != nil {
		return ctrl.Result{}, err
	}

	entitlementsResult, err := r.reconcileEntitlements(ctx, coderControlPlane, &nextStatus)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileStatus(ctx, coderControlPlane, originalStatus, nextStatus); err != nil {
		return ctrl.Result{}, err
	}

	result := mergeResults(operatorResult, licenseResult, entitlementsResult)
	if requiresWorkspaceRBACDriftRequeue(coderControlPlane) {
		result = mergeResults(result, ctrl.Result{RequeueAfter: workspaceRBACDriftRequeueInterval})
	}
	if gatewayExposureNeedsRequeue {
		result = mergeResults(result, ctrl.Result{RequeueAfter: gatewayExposureRequeueInterval})
	}

	return result, nil
}

func resolveServiceAccountName(cp *coderv1alpha1.CoderControlPlane) string {
	if cp.Spec.ServiceAccount.Name != "" {
		return cp.Spec.ServiceAccount.Name
	}
	return cp.Name
}

func controlPlaneTLSEnabled(cp *coderv1alpha1.CoderControlPlane) bool {
	if cp == nil {
		return false
	}
	return len(cp.Spec.TLS.SecretNames) > 0
}

func requiresWorkspaceRBACDriftRequeue(cp *coderv1alpha1.CoderControlPlane) bool {
	if cp == nil || !cp.Spec.RBAC.WorkspacePerms {
		return false
	}

	for _, namespace := range cp.Spec.RBAC.WorkspaceNamespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" || namespace == cp.Namespace {
			continue
		}
		return true
	}

	return false
}

func workspaceRBACLabels(cp *coderv1alpha1.CoderControlPlane) map[string]string {
	labels := maps.Clone(controlPlaneLabels(cp.Name))
	labels["coder.com/control-plane"] = cp.Name
	labels["coder.com/control-plane-namespace"] = cp.Namespace
	return labels
}

func workspaceRBACAnnotations(ownerUID string) map[string]string {
	return map[string]string{workspaceRBACOwnerUIDAnnotation: ownerUID}
}

func hasWorkspaceRBACIdentityLabels(object metav1.Object, coderControlPlane *coderv1alpha1.CoderControlPlane) bool {
	if object == nil || coderControlPlane == nil {
		return false
	}

	labels := object.GetLabels()
	if labels == nil {
		return false
	}

	return labels["coder.com/control-plane"] == coderControlPlane.Name &&
		labels["coder.com/control-plane-namespace"] == coderControlPlane.Namespace
}

func hasWorkspaceRBACOwnerUID(object metav1.Object, coderControlPlane *coderv1alpha1.CoderControlPlane) bool {
	if object == nil || coderControlPlane == nil {
		return false
	}

	ownerUID := strings.TrimSpace(string(coderControlPlane.UID))
	if ownerUID == "" {
		return false
	}

	annotations := object.GetAnnotations()
	if annotations == nil {
		return false
	}

	return strings.TrimSpace(annotations[workspaceRBACOwnerUIDAnnotation]) == ownerUID
}

func isManagedWorkspaceRole(
	role *rbacv1.Role,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	expectedRoleName string,
) bool {
	if role == nil || coderControlPlane == nil {
		return false
	}
	if isOwnedByCoderControlPlane(role, coderControlPlane) {
		return true
	}
	if !hasWorkspaceRBACIdentityLabels(role, coderControlPlane) {
		return false
	}
	if hasWorkspaceRBACOwnerUID(role, coderControlPlane) {
		return true
	}

	return role.Namespace != coderControlPlane.Namespace && role.Name == expectedRoleName
}

func isManagedWorkspaceRoleBinding(
	roleBinding *rbacv1.RoleBinding,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	expectedRoleName string,
	expectedRoleBindingName string,
	expectedServiceAccountName string,
) bool {
	if roleBinding == nil || coderControlPlane == nil {
		return false
	}
	if isOwnedByCoderControlPlane(roleBinding, coderControlPlane) {
		return true
	}
	if !hasWorkspaceRBACIdentityLabels(roleBinding, coderControlPlane) {
		return false
	}
	if hasWorkspaceRBACOwnerUID(roleBinding, coderControlPlane) {
		return true
	}
	if roleBinding.Namespace == coderControlPlane.Namespace {
		return false
	}
	if roleBinding.Name != expectedRoleBindingName {
		return false
	}
	if roleBinding.RoleRef.APIGroup != rbacv1.GroupName || roleBinding.RoleRef.Kind != "Role" || roleBinding.RoleRef.Name != expectedRoleName {
		return false
	}
	if len(roleBinding.Subjects) != 1 {
		return false
	}

	subject := roleBinding.Subjects[0]
	return subject.Kind == rbacv1.ServiceAccountKind &&
		subject.Name == expectedServiceAccountName &&
		subject.Namespace == coderControlPlane.Namespace
}

func (r *CoderControlPlaneReconciler) ensureWorkspaceRBACFinalizer(
	ctx context.Context,
	namespacedName types.NamespacedName,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
) error {
	if coderControlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}
	if coderControlPlane.Name != namespacedName.Name || coderControlPlane.Namespace != namespacedName.Namespace {
		return fmt.Errorf("assertion failed: finalizer target %s/%s does not match request %s", coderControlPlane.Namespace, coderControlPlane.Name, namespacedName)
	}
	if controllerutil.ContainsFinalizer(coderControlPlane, workspaceRBACFinalizer) {
		return nil
	}

	original := coderControlPlane.DeepCopy()
	controllerutil.AddFinalizer(coderControlPlane, workspaceRBACFinalizer)
	if err := r.Patch(ctx, coderControlPlane, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("add workspace RBAC finalizer: %w", err)
	}
	if err := r.Get(ctx, namespacedName, coderControlPlane); err != nil {
		return fmt.Errorf("reload codercontrolplane %s after finalizer update: %w", namespacedName, err)
	}

	return nil
}

func (r *CoderControlPlaneReconciler) finalizeWorkspaceRBAC(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
) (ctrl.Result, error) {
	if coderControlPlane == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: coder control plane must not be nil")
	}
	if !controllerutil.ContainsFinalizer(coderControlPlane, workspaceRBACFinalizer) {
		return ctrl.Result{}, nil
	}

	if err := r.cleanupManagedWorkspaceRBAC(ctx, coderControlPlane, nil, nil); err != nil {
		return ctrl.Result{}, err
	}

	original := coderControlPlane.DeepCopy()
	controllerutil.RemoveFinalizer(coderControlPlane, workspaceRBACFinalizer)
	if err := r.Patch(ctx, coderControlPlane, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove workspace RBAC finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *CoderControlPlaneReconciler) reconcileServiceAccount(ctx context.Context, coderControlPlane *coderv1alpha1.CoderControlPlane) error {
	if coderControlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}
	if coderControlPlane.Spec.ServiceAccount.DisableCreate {
		return r.detachManagedServiceAccounts(ctx, coderControlPlane)
	}

	serviceAccountName := resolveServiceAccountName(coderControlPlane)
	if strings.TrimSpace(serviceAccountName) == "" {
		return fmt.Errorf("assertion failed: service account name must not be empty")
	}

	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccountName, Namespace: coderControlPlane.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, serviceAccount, func() error {
		labels := maps.Clone(controlPlaneLabels(coderControlPlane.Name))
		maps.Copy(labels, coderControlPlane.Spec.ServiceAccount.Labels)
		serviceAccount.Labels = labels
		serviceAccount.Annotations = maps.Clone(coderControlPlane.Spec.ServiceAccount.Annotations)

		if err := controllerutil.SetControllerReference(coderControlPlane, serviceAccount, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("reconcile control plane serviceaccount: %w", err)
	}

	return nil
}

func (r *CoderControlPlaneReconciler) detachManagedServiceAccounts(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
) error {
	if coderControlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	serviceAccounts := &corev1.ServiceAccountList{}
	if err := r.List(
		ctx,
		serviceAccounts,
		client.InNamespace(coderControlPlane.Namespace),
		client.MatchingLabels(controlPlaneLabels(coderControlPlane.Name)),
	); err != nil {
		return fmt.Errorf("list managed service accounts: %w", err)
	}

	for i := range serviceAccounts.Items {
		serviceAccount := &serviceAccounts.Items[i]
		if !isOwnedByCoderControlPlane(serviceAccount, coderControlPlane) {
			continue
		}

		original := serviceAccount.DeepCopy()
		if err := controllerutil.RemoveControllerReference(coderControlPlane, serviceAccount, r.Scheme); err != nil {
			return fmt.Errorf("remove controller reference from service account %s/%s: %w", serviceAccount.Namespace, serviceAccount.Name, err)
		}
		if equality.Semantic.DeepEqual(original.OwnerReferences, serviceAccount.OwnerReferences) {
			continue
		}

		if err := r.Patch(ctx, serviceAccount, client.MergeFrom(original)); err != nil {
			return fmt.Errorf("patch detached service account %s/%s: %w", serviceAccount.Namespace, serviceAccount.Name, err)
		}
	}

	return nil
}

func (r *CoderControlPlaneReconciler) reconcileWorkspaceRBAC(ctx context.Context, coderControlPlane *coderv1alpha1.CoderControlPlane) error {
	if coderControlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	serviceAccountName := resolveServiceAccountName(coderControlPlane)
	if strings.TrimSpace(serviceAccountName) == "" {
		return fmt.Errorf("assertion failed: service account name must not be empty")
	}
	ownerUID := strings.TrimSpace(string(coderControlPlane.UID))
	if ownerUID == "" {
		return fmt.Errorf("assertion failed: coder control plane UID must not be empty")
	}
	roleName := fmt.Sprintf("%s-workspace-perms", serviceAccountName)
	roleBindingName := serviceAccountName

	if !coderControlPlane.Spec.RBAC.WorkspacePerms {
		return r.cleanupManagedWorkspaceRBAC(ctx, coderControlPlane, nil, nil)
	}

	rules := []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"persistentvolumeclaims"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"},
		},
	}
	if coderControlPlane.Spec.RBAC.EnableDeployments {
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{"apps"},
			Resources: []string{"deployments"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"},
		})
	}
	rules = append(rules, coderControlPlane.Spec.RBAC.ExtraRules...)

	targetNamespaces := append([]string{coderControlPlane.Namespace}, coderControlPlane.Spec.RBAC.WorkspaceNamespaces...)
	seenNamespaces := make(map[string]struct{}, len(targetNamespaces))
	keepRoles := make(map[string]struct{}, len(targetNamespaces))
	keepRoleBindings := make(map[string]struct{}, len(targetNamespaces))
	for _, namespace := range targetNamespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" {
			return fmt.Errorf("assertion failed: workspace namespace must not be empty")
		}
		if _, seen := seenNamespaces[namespace]; seen {
			continue
		}
		seenNamespaces[namespace] = struct{}{}

		labels := workspaceRBACLabels(coderControlPlane)
		annotations := workspaceRBACAnnotations(ownerUID)

		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: namespace}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
			role.Labels = maps.Clone(labels)
			role.Annotations = maps.Clone(annotations)
			role.Rules = append([]rbacv1.PolicyRule(nil), rules...)

			if namespace == coderControlPlane.Namespace {
				if err := controllerutil.SetControllerReference(coderControlPlane, role, r.Scheme); err != nil {
					return fmt.Errorf("set controller reference: %w", err)
				}
			} else {
				role.OwnerReferences = nil
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("reconcile workspace role %s/%s: %w", namespace, roleName, err)
		}

		roleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: roleBindingName, Namespace: namespace}}
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, roleBinding, func() error {
			roleBinding.Labels = maps.Clone(labels)
			roleBinding.Annotations = maps.Clone(annotations)
			roleBinding.RoleRef = rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "Role",
				Name:     roleName,
			}
			roleBinding.Subjects = []rbacv1.Subject{{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      serviceAccountName,
				Namespace: coderControlPlane.Namespace,
			}}

			if namespace == coderControlPlane.Namespace {
				if err := controllerutil.SetControllerReference(coderControlPlane, roleBinding, r.Scheme); err != nil {
					return fmt.Errorf("set controller reference: %w", err)
				}
			} else {
				roleBinding.OwnerReferences = nil
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("reconcile workspace role binding %s/%s: %w", namespace, roleBindingName, err)
		}

		keepRoles[namespacedResourceKey(namespace, roleName)] = struct{}{}
		keepRoleBindings[namespacedResourceKey(namespace, roleBindingName)] = struct{}{}
	}

	return r.cleanupManagedWorkspaceRBAC(ctx, coderControlPlane, keepRoles, keepRoleBindings)
}

func namespacedResourceKey(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

func (r *CoderControlPlaneReconciler) cleanupManagedWorkspaceRBAC(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	keepRoles map[string]struct{},
	keepRoleBindings map[string]struct{},
) error {
	if coderControlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	serviceAccountName := resolveServiceAccountName(coderControlPlane)
	if strings.TrimSpace(serviceAccountName) == "" {
		return fmt.Errorf("assertion failed: service account name must not be empty")
	}
	expectedRoleName := fmt.Sprintf("%s-workspace-perms", serviceAccountName)
	expectedRoleBindingName := serviceAccountName

	labels := workspaceRBACLabels(coderControlPlane)

	roles := &rbacv1.RoleList{}
	if err := r.List(ctx, roles, client.MatchingLabels(labels)); err != nil {
		return fmt.Errorf("list managed workspace roles: %w", err)
	}
	for i := range roles.Items {
		role := &roles.Items[i]
		if keepRoles != nil {
			if _, ok := keepRoles[namespacedResourceKey(role.Namespace, role.Name)]; ok {
				continue
			}
		}
		if !isManagedWorkspaceRole(role, coderControlPlane, expectedRoleName) {
			continue
		}
		if err := r.Delete(ctx, role); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete managed workspace role %s/%s: %w", role.Namespace, role.Name, err)
		}
	}

	roleBindings := &rbacv1.RoleBindingList{}
	if err := r.List(ctx, roleBindings, client.MatchingLabels(labels)); err != nil {
		return fmt.Errorf("list managed workspace role bindings: %w", err)
	}
	for i := range roleBindings.Items {
		roleBinding := &roleBindings.Items[i]
		if keepRoleBindings != nil {
			if _, ok := keepRoleBindings[namespacedResourceKey(roleBinding.Namespace, roleBinding.Name)]; ok {
				continue
			}
		}
		if !isManagedWorkspaceRoleBinding(roleBinding, coderControlPlane, expectedRoleName, expectedRoleBindingName, serviceAccountName) {
			continue
		}
		if err := r.Delete(ctx, roleBinding); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete managed workspace role binding %s/%s: %w", roleBinding.Namespace, roleBinding.Name, err)
		}
	}

	return nil
}

func buildProbe(spec coderv1alpha1.ProbeSpec, path, portName string) *corev1.Probe {
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   path,
				Port:   intstr.FromString(portName),
				Scheme: corev1.URISchemeHTTP,
			},
		},
		InitialDelaySeconds: spec.InitialDelaySeconds,
	}
	if spec.PeriodSeconds != nil {
		probe.PeriodSeconds = *spec.PeriodSeconds
	}
	if spec.TimeoutSeconds != nil {
		probe.TimeoutSeconds = *spec.TimeoutSeconds
	}
	if spec.SuccessThreshold != nil {
		probe.SuccessThreshold = *spec.SuccessThreshold
	}
	if spec.FailureThreshold != nil {
		probe.FailureThreshold = *spec.FailureThreshold
	}

	return probe
}

func (r *CoderControlPlaneReconciler) reconcileDeployment(ctx context.Context, coderControlPlane *coderv1alpha1.CoderControlPlane) (*appsv1.Deployment, error) {
	if coderControlPlane == nil {
		return nil, fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: coderControlPlane.Name, Namespace: coderControlPlane.Namespace}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		labels := controlPlaneLabels(coderControlPlane.Name)
		deployment.Labels = maps.Clone(labels)

		if err := controllerutil.SetControllerReference(coderControlPlane, deployment, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		replicas := int32(1)
		if coderControlPlane.Spec.Replicas != nil {
			replicas = *coderControlPlane.Spec.Replicas
		}

		image := coderControlPlane.Spec.Image
		if image == "" {
			image = defaultCoderImage
		}

		serviceAccountName := resolveServiceAccountName(coderControlPlane)
		if strings.TrimSpace(serviceAccountName) == "" {
			return fmt.Errorf("assertion failed: service account name must not be empty")
		}

		args := []string{"--http-address=0.0.0.0:8080"}
		args = append(args, coderControlPlane.Spec.ExtraArgs...)

		env := []corev1.EnvVar{
			{
				Name: "KUBE_POD_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
				},
			},
			{
				Name:  "CODER_DERP_SERVER_RELAY_URL",
				Value: "http://$(KUBE_POD_IP):8080",
			},
		}

		tlsEnabled := controlPlaneTLSEnabled(coderControlPlane)
		if coderControlPlane.Spec.EnvUseClusterAccessURL == nil || *coderControlPlane.Spec.EnvUseClusterAccessURL {
			configuredAccessURL, err := findEnvVar(coderControlPlane.Spec.ExtraEnv, "CODER_ACCESS_URL")
			if err != nil {
				return err
			}
			if configuredAccessURL == nil {
				scheme := "http"
				accessURLPort := coderControlPlane.Spec.Service.Port
				if accessURLPort == 0 {
					accessURLPort = defaultControlPlanePort
				}
				if tlsEnabled {
					scheme = "https"
					accessURLPort = 443
				}

				accessURL := fmt.Sprintf("%s://%s.%s.svc.cluster.local", scheme, coderControlPlane.Name, coderControlPlane.Namespace)
				if (scheme == "http" && accessURLPort != 80) || (scheme == "https" && accessURLPort != 443) {
					accessURL = fmt.Sprintf("%s:%d", accessURL, accessURLPort)
				}
				env = append(env, corev1.EnvVar{
					Name:  "CODER_ACCESS_URL",
					Value: accessURL,
				})
			}
		}

		ports := []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: controlPlaneTargetPort,
			Protocol:      corev1.ProtocolTCP,
		}}

		volumes := make([]corev1.Volume, 0, len(coderControlPlane.Spec.TLS.SecretNames)+len(coderControlPlane.Spec.Certs.Secrets)+len(coderControlPlane.Spec.Volumes))
		volumeMounts := make([]corev1.VolumeMount, 0, len(coderControlPlane.Spec.TLS.SecretNames)+len(coderControlPlane.Spec.Certs.Secrets)+len(coderControlPlane.Spec.VolumeMounts))
		if tlsEnabled {
			tlsCertFiles := make([]string, 0, len(coderControlPlane.Spec.TLS.SecretNames))
			tlsKeyFiles := make([]string, 0, len(coderControlPlane.Spec.TLS.SecretNames))

			for _, secretName := range coderControlPlane.Spec.TLS.SecretNames {
				secretName = strings.TrimSpace(secretName)
				if secretName == "" {
					return fmt.Errorf("assertion failed: tls secret name must not be empty")
				}

				volumeName := volumeNameForSecret("tls", secretName)
				mountPath := fmt.Sprintf("/etc/ssl/certs/coder/%s", secretName)
				volumes = append(volumes, corev1.Volume{
					Name: volumeName,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: secretName},
					},
				})
				volumeMounts = append(volumeMounts, corev1.VolumeMount{
					Name:      volumeName,
					MountPath: mountPath,
					ReadOnly:  true,
				})

				tlsCertFiles = append(tlsCertFiles, fmt.Sprintf("%s/tls.crt", mountPath))
				tlsKeyFiles = append(tlsKeyFiles, fmt.Sprintf("%s/tls.key", mountPath))
			}

			env = append(env,
				corev1.EnvVar{Name: "CODER_TLS_ENABLE", Value: "true"},
				corev1.EnvVar{Name: "CODER_TLS_ADDRESS", Value: "0.0.0.0:8443"},
				corev1.EnvVar{Name: "CODER_TLS_CERT_FILE", Value: strings.Join(tlsCertFiles, ",")},
				corev1.EnvVar{Name: "CODER_TLS_KEY_FILE", Value: strings.Join(tlsKeyFiles, ",")},
			)

			ports = append(ports, corev1.ContainerPort{
				Name:          "https",
				ContainerPort: controlPlaneTLSTargetPort,
				Protocol:      corev1.ProtocolTCP,
			})
		}

		for _, secret := range coderControlPlane.Spec.Certs.Secrets {
			secret.Name = strings.TrimSpace(secret.Name)
			secret.Key = strings.TrimSpace(secret.Key)
			if secret.Name == "" {
				return fmt.Errorf("assertion failed: cert secret name must not be empty")
			}
			if secret.Key == "" {
				return fmt.Errorf("assertion failed: cert secret key must not be empty")
			}

			volumeName := volumeNameForSecret("ca-cert", secret.Name)
			volumes = append(volumes, corev1.Volume{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: secret.Name},
				},
			})
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      volumeName,
				MountPath: fmt.Sprintf("/etc/ssl/certs/%s.crt", secret.Name),
				SubPath:   secret.Key,
				ReadOnly:  true,
			})
		}

		env = append(env, coderControlPlane.Spec.ExtraEnv...)
		volumes = append(volumes, coderControlPlane.Spec.Volumes...)
		volumeMounts = append(volumeMounts, coderControlPlane.Spec.VolumeMounts...)

		container := corev1.Container{
			Name:         "coder",
			Image:        image,
			Args:         args,
			Env:          env,
			EnvFrom:      coderControlPlane.Spec.EnvFrom,
			Ports:        ports,
			VolumeMounts: volumeMounts,
		}
		if coderControlPlane.Spec.SecurityContext != nil {
			container.SecurityContext = coderControlPlane.Spec.SecurityContext
		}
		if coderControlPlane.Spec.Resources != nil {
			container.Resources = *coderControlPlane.Spec.Resources
		}
		if coderControlPlane.Spec.ReadinessProbe.Enabled {
			container.ReadinessProbe = buildProbe(coderControlPlane.Spec.ReadinessProbe, "/healthz", "http")
		}
		if coderControlPlane.Spec.LivenessProbe.Enabled {
			container.LivenessProbe = buildProbe(coderControlPlane.Spec.LivenessProbe, "/healthz", "http")
		}

		podSpec := corev1.PodSpec{
			ServiceAccountName: serviceAccountName,
			ImagePullSecrets:   coderControlPlane.Spec.ImagePullSecrets,
			Containers:         []corev1.Container{container},
			Volumes:            volumes,
			NodeSelector:       maps.Clone(coderControlPlane.Spec.NodeSelector),
			Tolerations:        append([]corev1.Toleration(nil), coderControlPlane.Spec.Tolerations...),
			TopologySpreadConstraints: append(
				[]corev1.TopologySpreadConstraint(nil),
				coderControlPlane.Spec.TopologySpreadConstraints...,
			),
		}
		if coderControlPlane.Spec.PodSecurityContext != nil {
			podSpec.SecurityContext = coderControlPlane.Spec.PodSecurityContext
		}
		if coderControlPlane.Spec.Affinity != nil {
			podSpec.Affinity = coderControlPlane.Spec.Affinity
		}

		deployment.Spec.Replicas = &replicas
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: maps.Clone(labels)}
		deployment.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: maps.Clone(labels)},
			Spec:       podSpec,
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile control plane deployment: %w", err)
	}

	// Avoid an immediate cached read-after-write here; cache propagation lag can
	// transiently return NotFound for just-created objects and produce noisy reconcile errors.
	return deployment, nil
}

func (r *CoderControlPlaneReconciler) reconcileService(ctx context.Context, coderControlPlane *coderv1alpha1.CoderControlPlane) (*corev1.Service, error) {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: coderControlPlane.Name, Namespace: coderControlPlane.Namespace}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		labels := controlPlaneLabels(coderControlPlane.Name)
		service.Labels = maps.Clone(labels)
		service.Annotations = maps.Clone(coderControlPlane.Spec.Service.Annotations)

		if err := controllerutil.SetControllerReference(coderControlPlane, service, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		serviceType := coderControlPlane.Spec.Service.Type
		if serviceType == "" {
			serviceType = corev1.ServiceTypeClusterIP
		}
		servicePort := coderControlPlane.Spec.Service.Port
		if servicePort == 0 {
			servicePort = defaultControlPlanePort
		}

		tlsEnabled := controlPlaneTLSEnabled(coderControlPlane)
		primaryServicePort := corev1.ServicePort{
			Name:       "http",
			Port:       servicePort,
			Protocol:   corev1.ProtocolTCP,
			TargetPort: intstr.FromInt(int(controlPlaneTargetPort)),
		}
		if tlsEnabled && servicePort == 443 {
			primaryServicePort.Name = "https"
			primaryServicePort.TargetPort = intstr.FromInt(int(controlPlaneTLSTargetPort))
		}

		servicePorts := []corev1.ServicePort{primaryServicePort}
		if tlsEnabled && servicePort != 443 {
			servicePorts = append(servicePorts, corev1.ServicePort{
				Name:       "https",
				Port:       443,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt(int(controlPlaneTLSTargetPort)),
			})
		}

		service.Spec.Type = serviceType
		service.Spec.Selector = maps.Clone(labels)
		service.Spec.Ports = servicePorts
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile control plane service: %w", err)
	}

	// Avoid an immediate cached read-after-write here; cache propagation lag can
	// transiently return NotFound for just-created objects and produce noisy reconcile errors.
	return service, nil
}

func (r *CoderControlPlaneReconciler) reconcileExposure(ctx context.Context, coderControlPlane *coderv1alpha1.CoderControlPlane) (bool, error) {
	if coderControlPlane == nil {
		return false, fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	exposeSpec := coderControlPlane.Spec.Expose
	if exposeSpec == nil || (exposeSpec.Ingress == nil && exposeSpec.Gateway == nil) {
		if err := r.cleanupOwnedIngress(ctx, coderControlPlane); err != nil {
			return false, fmt.Errorf("cleanup managed ingress: %w", err)
		}
		if err := r.cleanupOwnedHTTPRoute(ctx, coderControlPlane); err != nil {
			return false, fmt.Errorf("cleanup managed httproute: %w", err)
		}
		return false, nil
	}

	if exposeSpec.Ingress != nil && exposeSpec.Gateway != nil {
		return false, fmt.Errorf("assertion failed: only one of ingress or gateway exposure may be configured")
	}

	if exposeSpec.Ingress != nil {
		if err := r.reconcileIngress(ctx, coderControlPlane); err != nil {
			return false, err
		}
		if err := r.cleanupOwnedHTTPRoute(ctx, coderControlPlane); err != nil {
			return false, fmt.Errorf("cleanup managed httproute: %w", err)
		}
		return false, nil
	}

	httpRouteReconciled, err := r.reconcileHTTPRoute(ctx, coderControlPlane)
	if err != nil {
		return false, err
	}
	if err := r.cleanupOwnedIngress(ctx, coderControlPlane); err != nil {
		return false, fmt.Errorf("cleanup managed ingress: %w", err)
	}

	return httpRouteReconciled, nil
}

func (r *CoderControlPlaneReconciler) reconcileIngress(ctx context.Context, coderControlPlane *coderv1alpha1.CoderControlPlane) error {
	if coderControlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}
	if coderControlPlane.Spec.Expose == nil || coderControlPlane.Spec.Expose.Ingress == nil {
		return fmt.Errorf("assertion failed: ingress exposure spec must not be nil")
	}

	ingressExpose := coderControlPlane.Spec.Expose.Ingress
	primaryHost := strings.TrimSpace(ingressExpose.Host)
	if primaryHost == "" {
		return fmt.Errorf("assertion failed: ingress host must not be empty")
	}

	wildcardHost := strings.TrimSpace(ingressExpose.WildcardHost)
	servicePort := coderControlPlane.Spec.Service.Port
	if servicePort == 0 {
		servicePort = defaultControlPlanePort
	}

	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: coderControlPlane.Name, Namespace: coderControlPlane.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ingress, func() error {
		labels := controlPlaneLabels(coderControlPlane.Name)
		ingress.Labels = maps.Clone(labels)
		ingress.Annotations = maps.Clone(ingressExpose.Annotations)

		pathTypePrefix := networkingv1.PathTypePrefix
		rules := []networkingv1.IngressRule{
			{
				Host: primaryHost,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{
								Path:     "/",
								PathType: &pathTypePrefix,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: coderControlPlane.Name,
										Port: networkingv1.ServiceBackendPort{Number: servicePort},
									},
								},
							},
						},
					},
				},
			},
		}
		if wildcardHost != "" {
			rules = append(rules, networkingv1.IngressRule{
				Host: wildcardHost,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{
								Path:     "/",
								PathType: &pathTypePrefix,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: coderControlPlane.Name,
										Port: networkingv1.ServiceBackendPort{Number: servicePort},
									},
								},
							},
						},
					},
				},
			})
		}

		var tls []networkingv1.IngressTLS
		if ingressExpose.TLS != nil {
			secretName := strings.TrimSpace(ingressExpose.TLS.SecretName)
			if secretName != "" {
				tls = append(tls, networkingv1.IngressTLS{
					SecretName: secretName,
					Hosts:      []string{primaryHost},
				})
			}

			wildcardSecretName := strings.TrimSpace(ingressExpose.TLS.WildcardSecretName)
			if wildcardSecretName != "" {
				if wildcardHost == "" {
					return fmt.Errorf("assertion failed: ingress wildcard host must not be empty when wildcard TLS secret is set")
				}
				tls = append(tls, networkingv1.IngressTLS{
					SecretName: wildcardSecretName,
					Hosts:      []string{wildcardHost},
				})
			}
		}

		ingress.Spec = networkingv1.IngressSpec{
			IngressClassName: ingressExpose.ClassName,
			Rules:            rules,
			TLS:              tls,
		}

		if err := controllerutil.SetControllerReference(coderControlPlane, ingress, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("reconcile control plane ingress: %w", err)
	}

	return nil
}

func (r *CoderControlPlaneReconciler) reconcileHTTPRoute(ctx context.Context, coderControlPlane *coderv1alpha1.CoderControlPlane) (bool, error) {
	if coderControlPlane == nil {
		return false, fmt.Errorf("assertion failed: coder control plane must not be nil")
	}
	if coderControlPlane.Spec.Expose == nil || coderControlPlane.Spec.Expose.Gateway == nil {
		return false, fmt.Errorf("assertion failed: gateway exposure spec must not be nil")
	}

	gatewayExpose := coderControlPlane.Spec.Expose.Gateway
	primaryHost := strings.TrimSpace(gatewayExpose.Host)
	if primaryHost == "" {
		return false, fmt.Errorf("assertion failed: gateway host must not be empty")
	}

	httpRoute := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: coderControlPlane.Name, Namespace: coderControlPlane.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, httpRoute, func() error {
		labels := controlPlaneLabels(coderControlPlane.Name)
		httpRoute.Labels = maps.Clone(labels)

		parentRefs := make([]gatewayv1.ParentReference, 0, len(gatewayExpose.ParentRefs))
		for i := range gatewayExpose.ParentRefs {
			parentRefSpec := gatewayExpose.ParentRefs[i]
			parentRefName := strings.TrimSpace(parentRefSpec.Name)
			if parentRefName == "" {
				return fmt.Errorf("assertion failed: gateway parentRef[%d] name must not be empty", i)
			}

			parentRef := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(parentRefName)}
			if parentRefSpec.Namespace != nil {
				namespace := strings.TrimSpace(*parentRefSpec.Namespace)
				if namespace == "" {
					return fmt.Errorf("assertion failed: gateway parentRef[%d] namespace must not be empty when set", i)
				}
				namespaceRef := gatewayv1.Namespace(namespace)
				parentRef.Namespace = &namespaceRef
			}
			if parentRefSpec.SectionName != nil {
				sectionName := strings.TrimSpace(*parentRefSpec.SectionName)
				if sectionName == "" {
					return fmt.Errorf("assertion failed: gateway parentRef[%d] sectionName must not be empty when set", i)
				}
				sectionNameRef := gatewayv1.SectionName(sectionName)
				parentRef.SectionName = &sectionNameRef
			}

			parentRefs = append(parentRefs, parentRef)
		}

		hostnames := []gatewayv1.Hostname{gatewayv1.Hostname(primaryHost)}
		wildcardHost := strings.TrimSpace(gatewayExpose.WildcardHost)
		if wildcardHost != "" {
			hostnames = append(hostnames, gatewayv1.Hostname(wildcardHost))
		}

		servicePort := coderControlPlane.Spec.Service.Port
		if servicePort == 0 {
			servicePort = defaultControlPlanePort
		}
		backendPort := gatewayv1.PortNumber(servicePort)
		serviceKind := gatewayv1.Kind("Service")
		serviceGroup := gatewayv1.Group("")
		pathTypePrefix := gatewayv1.PathMatchPathPrefix
		pathPrefix := "/"

		httpRoute.Spec = gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefs},
			Hostnames:       hostnames,
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path: &gatewayv1.HTTPPathMatch{
								Type:  &pathTypePrefix,
								Value: &pathPrefix,
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Group: &serviceGroup,
									Kind:  &serviceKind,
									Name:  gatewayv1.ObjectName(coderControlPlane.Name),
									Port:  &backendPort,
								},
							},
						},
					},
				},
			},
		}

		if err := controllerutil.SetControllerReference(coderControlPlane, httpRoute, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		return nil
	})
	if err != nil {
		if meta.IsNoMatchError(err) {
			ctrl.LoggerFrom(ctx).WithName("controller").WithName("codercontrolplane").Info(
				"Gateway API CRDs not available, skipping HTTPRoute reconciliation",
			)
			return false, nil
		}
		return false, fmt.Errorf("reconcile control plane httproute: %w", err)
	}

	return true, nil
}

func (r *CoderControlPlaneReconciler) cleanupOwnedIngress(ctx context.Context, coderControlPlane *coderv1alpha1.CoderControlPlane) error {
	if coderControlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	ingress := &networkingv1.Ingress{}
	namespacedName := types.NamespacedName{Name: coderControlPlane.Name, Namespace: coderControlPlane.Namespace}
	err := r.Get(ctx, namespacedName, ingress)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		return nil
	default:
		return fmt.Errorf("get control plane ingress %s: %w", namespacedName, err)
	}

	if !isOwnedByCoderControlPlane(ingress, coderControlPlane) {
		return nil
	}

	if err := r.Delete(ctx, ingress); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete control plane ingress %s: %w", namespacedName, err)
	}

	return nil
}

func (r *CoderControlPlaneReconciler) cleanupOwnedHTTPRoute(ctx context.Context, coderControlPlane *coderv1alpha1.CoderControlPlane) error {
	if coderControlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	httpRoute := &gatewayv1.HTTPRoute{}
	namespacedName := types.NamespacedName{Name: coderControlPlane.Name, Namespace: coderControlPlane.Namespace}
	err := r.Get(ctx, namespacedName, httpRoute)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return nil
	default:
		return fmt.Errorf("get control plane httproute %s: %w", namespacedName, err)
	}

	if !isOwnedByCoderControlPlane(httpRoute, coderControlPlane) {
		return nil
	}

	if err := r.Delete(ctx, httpRoute); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return fmt.Errorf("delete control plane httproute %s: %w", namespacedName, err)
	}

	return nil
}

func isOwnedByCoderControlPlane(object metav1.Object, coderControlPlane *coderv1alpha1.CoderControlPlane) bool {
	if object == nil || coderControlPlane == nil {
		return false
	}

	ownerReference := metav1.GetControllerOf(object)
	if ownerReference == nil {
		return false
	}

	return ownerReference.APIVersion == coderv1alpha1.GroupVersion.String() &&
		ownerReference.Kind == "CoderControlPlane" &&
		ownerReference.Name == coderControlPlane.Name &&
		ownerReference.UID == coderControlPlane.UID
}

func (r *CoderControlPlaneReconciler) desiredStatus(
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	deployment *appsv1.Deployment,
	service *corev1.Service,
) coderv1alpha1.CoderControlPlaneStatus {
	nextStatus := coderControlPlane.Status

	servicePort := coderControlPlane.Spec.Service.Port
	if servicePort == 0 {
		servicePort = defaultControlPlanePort
	}

	phase := coderv1alpha1.CoderControlPlanePhasePending
	if deployment.Status.ReadyReplicas > 0 {
		phase = coderv1alpha1.CoderControlPlanePhaseReady
	}

	scheme := "http"
	statusPort := servicePort
	if controlPlaneTLSEnabled(coderControlPlane) {
		scheme = "https"
		statusPort = 443
	}

	nextStatus.ObservedGeneration = coderControlPlane.Generation
	nextStatus.ReadyReplicas = deployment.Status.ReadyReplicas
	nextStatus.URL = fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", scheme, service.Name, service.Namespace, statusPort)
	nextStatus.Phase = phase

	return nextStatus
}

func controlPlaneSDKURL(coderControlPlane *coderv1alpha1.CoderControlPlane) string {
	if coderControlPlane == nil {
		return ""
	}

	servicePort := coderControlPlane.Spec.Service.Port
	if servicePort == 0 {
		servicePort = defaultControlPlanePort
	}

	scheme := "http"
	if controlPlaneTLSEnabled(coderControlPlane) && servicePort == 443 {
		scheme = "https"
	}

	return fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", scheme, coderControlPlane.Name, coderControlPlane.Namespace, servicePort)
}

func (r *CoderControlPlaneReconciler) reconcileOperatorAccess(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	nextStatus *coderv1alpha1.CoderControlPlaneStatus,
) (ctrl.Result, error) {
	if coderControlPlane == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: coder control plane must not be nil")
	}
	if nextStatus == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: next status must not be nil")
	}

	if coderControlPlane.Spec.OperatorAccess.Disabled {
		cleanupErr := r.cleanupDisabledOperatorAccess(ctx, coderControlPlane)
		nextStatus.OperatorAccessReady = false
		if cleanupErr != nil {
			pendingSecretName := operatorAccessTokenSecretName(coderControlPlane)
			if strings.TrimSpace(pendingSecretName) == "" {
				return ctrl.Result{}, fmt.Errorf("assertion failed: operator token secret name must not be empty")
			}
			nextStatus.OperatorTokenSecretRef = &coderv1alpha1.SecretKeySelector{
				Name: pendingSecretName,
				Key:  coderv1alpha1.DefaultTokenSecretKey,
			}
			//nolint:nilerr // disabling operator access should retry cleanup without surfacing a terminal reconcile error.
			return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
		}
		nextStatus.OperatorTokenSecretRef = nil
		return ctrl.Result{}, nil
	}

	if r.OperatorAccessProvisioner == nil {
		nextStatus.OperatorTokenSecretRef = nil
		nextStatus.OperatorAccessReady = false
		return ctrl.Result{}, nil
	}

	operatorTokenSecretName := operatorAccessTokenSecretName(coderControlPlane)
	if strings.TrimSpace(operatorTokenSecretName) == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: operator token secret name must not be empty")
	}

	operatorTokenName := operatorAccessDatabaseTokenName(coderControlPlane)
	if strings.TrimSpace(operatorTokenName) == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: operator token name must not be empty")
	}

	existingToken, err := r.readSecretValue(ctx, coderControlPlane.Namespace, operatorTokenSecretName, coderv1alpha1.DefaultTokenSecretKey)
	switch {
	case err == nil:
		// Existing token is still validated by the provisioner to avoid stale or expired credentials.
	case apierrors.IsNotFound(err), errors.Is(err, errSecretValueMissing), errors.Is(err, errSecretValueEmpty):
		existingToken = ""
	default:
		return ctrl.Result{}, fmt.Errorf("read operator token secret %q: %w", operatorTokenSecretName, err)
	}

	postgresURL, resolveErr := r.resolvePostgresURLFromExtraEnv(ctx, coderControlPlane)
	if resolveErr != nil {
		nextStatus.OperatorTokenSecretRef = nil
		nextStatus.OperatorAccessReady = false
		//nolint:nilerr // missing bootstrap inputs should requeue without surfacing a terminal reconcile error.
		return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
	}

	token, provisionErr := r.OperatorAccessProvisioner.EnsureOperatorToken(ctx, coderbootstrap.EnsureOperatorTokenRequest{
		PostgresURL:      postgresURL,
		OperatorUsername: defaultOperatorAccessUsername,
		OperatorEmail:    defaultOperatorAccessEmail,
		TokenName:        operatorTokenName,
		TokenLifetime:    defaultOperatorAccessTokenLifetime,
		ExistingToken:    existingToken,
	})
	if provisionErr != nil {
		nextStatus.OperatorTokenSecretRef = nil
		nextStatus.OperatorAccessReady = false
		//nolint:nilerr // transient provisioning errors should requeue without surfacing a terminal reconcile error.
		return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
	}
	if token == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: operator access provisioner returned empty token")
	}

	if err := r.ensureOperatorTokenSecret(
		ctx,
		coderControlPlane,
		operatorTokenSecretName,
		coderv1alpha1.DefaultTokenSecretKey,
		token,
	); err != nil {
		return ctrl.Result{}, err
	}

	nextStatus.OperatorTokenSecretRef = &coderv1alpha1.SecretKeySelector{
		Name: operatorTokenSecretName,
		Key:  coderv1alpha1.DefaultTokenSecretKey,
	}
	nextStatus.OperatorAccessReady = true

	return ctrl.Result{}, nil
}

func (r *CoderControlPlaneReconciler) reconcileLicense(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	nextStatus *coderv1alpha1.CoderControlPlaneStatus,
) (ctrl.Result, error) {
	if coderControlPlane == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: coder control plane must not be nil")
	}
	if nextStatus == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: next status must not be nil")
	}

	if coderControlPlane.Spec.LicenseSecretRef == nil {
		if err := setControlPlaneCondition(
			nextStatus,
			coderControlPlane.Generation,
			coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
			metav1.ConditionUnknown,
			licenseConditionReasonPending,
			"License Secret reference is not configured.",
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if r.LicenseUploader == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: license uploader must not be nil when licenseSecretRef is configured")
	}

	if nextStatus.Phase != coderv1alpha1.CoderControlPlanePhaseReady {
		if err := setControlPlaneCondition(
			nextStatus,
			coderControlPlane.Generation,
			coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
			metav1.ConditionFalse,
			licenseConditionReasonPending,
			"Waiting for control plane readiness before applying license.",
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if !nextStatus.OperatorAccessReady || nextStatus.OperatorTokenSecretRef == nil {
		if err := setControlPlaneCondition(
			nextStatus,
			coderControlPlane.Generation,
			coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
			metav1.ConditionFalse,
			licenseConditionReasonPending,
			"Waiting for operator access credentials before applying license.",
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	controlPlaneURL := controlPlaneSDKURL(coderControlPlane)
	if strings.TrimSpace(controlPlaneURL) == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: control plane SDK URL must not be empty when licenseSecretRef is configured")
	}

	operatorTokenSecretName := strings.TrimSpace(nextStatus.OperatorTokenSecretRef.Name)
	if operatorTokenSecretName == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: operator token secret name must not be empty when operator access is ready")
	}
	operatorTokenSecretKey := strings.TrimSpace(nextStatus.OperatorTokenSecretRef.Key)
	if operatorTokenSecretKey == "" {
		operatorTokenSecretKey = coderv1alpha1.DefaultTokenSecretKey
	}

	operatorToken, err := r.readSecretValue(ctx, coderControlPlane.Namespace, operatorTokenSecretName, operatorTokenSecretKey)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err), errors.Is(err, errSecretValueMissing), errors.Is(err, errSecretValueEmpty):
		if err := setControlPlaneCondition(
			nextStatus,
			coderControlPlane.Generation,
			coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
			metav1.ConditionFalse,
			licenseConditionReasonSecretMissing,
			"Operator token Secret is missing or incomplete; retrying license upload.",
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
	default:
		if err := setControlPlaneCondition(
			nextStatus,
			coderControlPlane.Generation,
			coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
			metav1.ConditionFalse,
			licenseConditionReasonError,
			"Failed to read operator token Secret; retrying license upload.",
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
	}

	licenseSecretName := strings.TrimSpace(coderControlPlane.Spec.LicenseSecretRef.Name)
	if licenseSecretName == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: license secret name must not be empty when licenseSecretRef is configured")
	}
	licenseSecretKey := strings.TrimSpace(coderControlPlane.Spec.LicenseSecretRef.Key)
	if licenseSecretKey == "" {
		licenseSecretKey = coderv1alpha1.DefaultLicenseSecretKey
	}

	licenseJWT, err := r.readSecretValue(ctx, coderControlPlane.Namespace, licenseSecretName, licenseSecretKey)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err), errors.Is(err, errSecretValueMissing), errors.Is(err, errSecretValueEmpty):
		if err := setControlPlaneCondition(
			nextStatus,
			coderControlPlane.Generation,
			coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
			metav1.ConditionFalse,
			licenseConditionReasonSecretMissing,
			"License Secret is missing or incomplete; retrying upload.",
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
	default:
		if err := setControlPlaneCondition(
			nextStatus,
			coderControlPlane.Generation,
			coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
			metav1.ConditionFalse,
			licenseConditionReasonError,
			"Failed to read license Secret; retrying upload.",
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
	}

	licenseJWT = strings.TrimSpace(licenseJWT)
	if licenseJWT == "" {
		if err := setControlPlaneCondition(
			nextStatus,
			coderControlPlane.Generation,
			coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
			metav1.ConditionFalse,
			licenseConditionReasonSecretMissing,
			"License Secret value is empty after trimming whitespace.",
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
	}

	licenseHash, err := hashLicenseJWT(licenseJWT)
	if err != nil {
		return ctrl.Result{}, err
	}

	if nextStatus.LicenseLastApplied != nil && nextStatus.LicenseLastAppliedHash == licenseHash {
		hasAnyLicense, hasLicenseErr := r.LicenseUploader.HasAnyLicense(ctx, controlPlaneURL, operatorToken)
		if hasLicenseErr != nil {
			var sdkErr *codersdk.Error
			if errors.As(hasLicenseErr, &sdkErr) {
				switch sdkErr.StatusCode() {
				case http.StatusNotFound:
					if err := setControlPlaneCondition(
						nextStatus,
						coderControlPlane.Generation,
						coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
						metav1.ConditionFalse,
						licenseConditionReasonNotSupported,
						"Control plane does not expose the Enterprise licenses API.",
					); err != nil {
						return ctrl.Result{}, err
					}
					return ctrl.Result{}, nil
				case http.StatusUnauthorized, http.StatusForbidden:
					if err := setControlPlaneCondition(
						nextStatus,
						coderControlPlane.Generation,
						coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
						metav1.ConditionFalse,
						licenseConditionReasonForbidden,
						"Operator token is not authorized to query configured licenses.",
					); err != nil {
						return ctrl.Result{}, err
					}
					return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
				}
			}
			if err := setControlPlaneCondition(
				nextStatus,
				coderControlPlane.Generation,
				coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
				metav1.ConditionFalse,
				licenseConditionReasonError,
				"Failed to query existing licenses; retrying.",
			); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
		}
		if hasAnyLicense {
			if err := setControlPlaneCondition(
				nextStatus,
				coderControlPlane.Generation,
				coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
				metav1.ConditionTrue,
				licenseConditionReasonApplied,
				"Configured license is already applied.",
			); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	if err := r.LicenseUploader.AddLicense(ctx, controlPlaneURL, operatorToken, licenseJWT); err != nil {
		if isDuplicateLicenseUploadError(err) {
			now := metav1.Now()
			nextStatus.LicenseLastApplied = &now
			nextStatus.LicenseLastAppliedHash = licenseHash
			if err := setControlPlaneCondition(
				nextStatus,
				coderControlPlane.Generation,
				coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
				metav1.ConditionTrue,
				licenseConditionReasonApplied,
				"Configured license already exists in coderd.",
			); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		var sdkErr *codersdk.Error
		if errors.As(err, &sdkErr) {
			switch sdkErr.StatusCode() {
			case http.StatusNotFound:
				if err := setControlPlaneCondition(
					nextStatus,
					coderControlPlane.Generation,
					coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
					metav1.ConditionFalse,
					licenseConditionReasonNotSupported,
					"Control plane does not expose the Enterprise licenses API.",
				); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			case http.StatusUnauthorized, http.StatusForbidden:
				if err := setControlPlaneCondition(
					nextStatus,
					coderControlPlane.Generation,
					coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
					metav1.ConditionFalse,
					licenseConditionReasonForbidden,
					"Operator token is not authorized to upload the configured license.",
				); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
			}
		}
		if err := setControlPlaneCondition(
			nextStatus,
			coderControlPlane.Generation,
			coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
			metav1.ConditionFalse,
			licenseConditionReasonError,
			"Failed to upload configured license; retrying.",
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
	}

	now := metav1.Now()
	nextStatus.LicenseLastApplied = &now
	nextStatus.LicenseLastAppliedHash = licenseHash
	if err := setControlPlaneCondition(
		nextStatus,
		coderControlPlane.Generation,
		coderv1alpha1.CoderControlPlaneConditionLicenseApplied,
		metav1.ConditionTrue,
		licenseConditionReasonApplied,
		"Configured license uploaded successfully.",
	); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *CoderControlPlaneReconciler) reconcileEntitlements(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	nextStatus *coderv1alpha1.CoderControlPlaneStatus,
) (ctrl.Result, error) {
	if coderControlPlane == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: coder control plane must not be nil")
	}
	if nextStatus == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: next status must not be nil")
	}

	if strings.TrimSpace(nextStatus.LicenseTier) == "" {
		nextStatus.LicenseTier = coderv1alpha1.CoderControlPlaneLicenseTierUnknown
	}
	if strings.TrimSpace(nextStatus.ExternalProvisionerDaemonsEntitlement) == "" {
		nextStatus.ExternalProvisionerDaemonsEntitlement = coderv1alpha1.CoderControlPlaneEntitlementUnknown
	}

	if nextStatus.Phase != coderv1alpha1.CoderControlPlanePhaseReady ||
		!nextStatus.OperatorAccessReady ||
		nextStatus.OperatorTokenSecretRef == nil {
		return ctrl.Result{}, nil
	}
	controlPlaneURL := controlPlaneSDKURL(coderControlPlane)
	if strings.TrimSpace(controlPlaneURL) == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: control plane SDK URL must not be empty when querying entitlements")
	}
	if r.EntitlementsInspector == nil {
		return ctrl.Result{}, nil
	}

	operatorTokenSecretName := strings.TrimSpace(nextStatus.OperatorTokenSecretRef.Name)
	if operatorTokenSecretName == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: operator token secret name must not be empty when querying entitlements")
	}
	operatorTokenSecretKey := strings.TrimSpace(nextStatus.OperatorTokenSecretRef.Key)
	if operatorTokenSecretKey == "" {
		operatorTokenSecretKey = coderv1alpha1.DefaultTokenSecretKey
	}

	operatorToken, err := r.readSecretValue(ctx, coderControlPlane.Namespace, operatorTokenSecretName, operatorTokenSecretKey)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err), errors.Is(err, errSecretValueMissing), errors.Is(err, errSecretValueEmpty):
		return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
	default:
		return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
	}

	entitlements, err := r.EntitlementsInspector.Entitlements(ctx, controlPlaneURL, operatorToken)
	if err != nil {
		var sdkErr *codersdk.Error
		if errors.As(err, &sdkErr) {
			switch sdkErr.StatusCode() {
			case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
				return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
			}
		}
		return ctrl.Result{RequeueAfter: operatorAccessRetryInterval}, nil
	}
	if entitlements.Features == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: entitlements features must not be nil")
	}

	previousTier := nextStatus.LicenseTier
	previousExternalProvisionerEntitlement := nextStatus.ExternalProvisionerDaemonsEntitlement

	nextStatus.LicenseTier = licenseTierFromEntitlements(entitlements)
	nextStatus.ExternalProvisionerDaemonsEntitlement = externalProvisionerDaemonsEntitlement(entitlements)

	shouldRefreshEntitlementsTimestamp := nextStatus.EntitlementsLastChecked == nil
	if !shouldRefreshEntitlementsTimestamp {
		elapsedSinceLastCheck := time.Since(nextStatus.EntitlementsLastChecked.Time)
		shouldRefreshEntitlementsTimestamp = elapsedSinceLastCheck < 0 || elapsedSinceLastCheck >= entitlementsStatusRefreshInterval
	}
	if previousTier != nextStatus.LicenseTier ||
		previousExternalProvisionerEntitlement != nextStatus.ExternalProvisionerDaemonsEntitlement {
		shouldRefreshEntitlementsTimestamp = true
	}
	if shouldRefreshEntitlementsTimestamp {
		now := metav1.Now()
		nextStatus.EntitlementsLastChecked = &now
	}

	requeueAfter := entitlementsStatusRefreshInterval
	if nextStatus.EntitlementsLastChecked != nil {
		elapsedSinceLastCheck := time.Since(nextStatus.EntitlementsLastChecked.Time)
		if elapsedSinceLastCheck >= 0 && elapsedSinceLastCheck < entitlementsStatusRefreshInterval {
			requeueAfter = entitlementsStatusRefreshInterval - elapsedSinceLastCheck
		}
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func externalProvisionerDaemonsEntitlement(entitlements codersdk.Entitlements) string {
	feature, ok := entitlements.Features[codersdk.FeatureExternalProvisionerDaemons]
	if !ok {
		return coderv1alpha1.CoderControlPlaneEntitlementUnknown
	}

	return normalizedEntitlementValue(feature.Entitlement)
}

func normalizedEntitlementValue(entitlement codersdk.Entitlement) string {
	switch entitlement {
	case codersdk.EntitlementEntitled, codersdk.EntitlementGracePeriod, codersdk.EntitlementNotEntitled:
		return string(entitlement)
	default:
		return coderv1alpha1.CoderControlPlaneEntitlementUnknown
	}
}

func licenseTierFromEntitlements(entitlements codersdk.Entitlements) string {
	if !entitlements.HasLicense {
		return coderv1alpha1.CoderControlPlaneLicenseTierNone
	}
	if entitlements.Trial {
		return coderv1alpha1.CoderControlPlaneLicenseTierTrial
	}

	for _, featureName := range []codersdk.FeatureName{
		codersdk.FeatureCustomRoles,
		codersdk.FeatureMultipleOrganizations,
	} {
		feature, ok := entitlements.Features[featureName]
		if !ok {
			continue
		}
		if feature.Entitlement.Entitled() {
			return coderv1alpha1.CoderControlPlaneLicenseTierPremium
		}
	}

	return coderv1alpha1.CoderControlPlaneLicenseTierEnterprise
}

func (r *CoderControlPlaneReconciler) cleanupDisabledOperatorAccess(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
) error {
	if coderControlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	operatorTokenSecretName := operatorAccessTokenSecretName(coderControlPlane)
	if strings.TrimSpace(operatorTokenSecretName) == "" {
		return fmt.Errorf("assertion failed: operator token secret name must not be empty")
	}

	operatorTokenName := operatorAccessDatabaseTokenName(coderControlPlane)
	if strings.TrimSpace(operatorTokenName) == "" {
		return fmt.Errorf("assertion failed: operator token name must not be empty")
	}

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: operatorTokenSecretName, Namespace: coderControlPlane.Namespace}, secret)
	secretExists := false
	switch {
	case err == nil:
		secretExists = true
	case apierrors.IsNotFound(err):
		secretExists = false
	default:
		return fmt.Errorf("get operator token secret %q while disabling operator access: %w", operatorTokenSecretName, err)
	}

	managedSecretExists := secretExists && isManagedOperatorTokenSecret(secret, coderControlPlane)
	cleanupRequired := managedSecretExists || coderControlPlane.Status.OperatorTokenSecretRef != nil

	if managedSecretExists {
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete operator token secret %q while disabling operator access: %w", operatorTokenSecretName, err)
		}
	}

	postgresURL, err := r.resolvePostgresURLFromExtraEnv(ctx, coderControlPlane)
	if err != nil {
		if cleanupRequired {
			return fmt.Errorf("resolve postgres URL while disabling operator access: %w", err)
		}
		return nil
	}

	if r.OperatorAccessProvisioner == nil {
		return fmt.Errorf("assertion failed: operator access provisioner must not be nil while disabling managed credentials")
	}
	if err := r.OperatorAccessProvisioner.RevokeOperatorToken(ctx, coderbootstrap.RevokeOperatorTokenRequest{
		PostgresURL:      postgresURL,
		OperatorUsername: defaultOperatorAccessUsername,
		TokenName:        operatorTokenName,
	}); err != nil {
		return fmt.Errorf("revoke operator token while disabling operator access: %w", err)
	}

	return nil
}

func isManagedOperatorTokenSecret(secret *corev1.Secret, coderControlPlane *coderv1alpha1.CoderControlPlane) bool {
	return isOwnedByCoderControlPlane(secret, coderControlPlane)
}

func (r *CoderControlPlaneReconciler) resolvePostgresURLFromExtraEnv(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
) (string, error) {
	if coderControlPlane == nil {
		return "", fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	pgEnvVar, err := findEnvVar(coderControlPlane.Spec.ExtraEnv, postgresConnectionURLEnvVar)
	if err != nil {
		return "", err
	}
	if pgEnvVar == nil {
		return "", fmt.Errorf("%s is not configured", postgresConnectionURLEnvVar)
	}

	if value := strings.TrimSpace(pgEnvVar.Value); value != "" {
		return value, nil
	}

	if pgEnvVar.ValueFrom == nil {
		return "", fmt.Errorf("%s must define either value or valueFrom.secretKeyRef", postgresConnectionURLEnvVar)
	}
	if pgEnvVar.ValueFrom.SecretKeyRef == nil {
		return "", fmt.Errorf("%s valueFrom must be a secretKeyRef", postgresConnectionURLEnvVar)
	}

	secretRef := pgEnvVar.ValueFrom.SecretKeyRef
	if strings.TrimSpace(secretRef.Name) == "" {
		return "", fmt.Errorf("%s secretKeyRef name must not be empty", postgresConnectionURLEnvVar)
	}
	if strings.TrimSpace(secretRef.Key) == "" {
		return "", fmt.Errorf("%s secretKeyRef key must not be empty", postgresConnectionURLEnvVar)
	}

	return r.readSecretValue(ctx, coderControlPlane.Namespace, secretRef.Name, secretRef.Key)
}

func findEnvVar(envVars []corev1.EnvVar, name string) (*corev1.EnvVar, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("assertion failed: environment variable name must not be empty")
	}

	var found *corev1.EnvVar
	for i := range envVars {
		if envVars[i].Name != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%s is configured more than once", name)
		}

		envVarCopy := envVars[i]
		found = &envVarCopy
	}

	return found, nil
}

func operatorAccessDatabaseTokenName(coderControlPlane *coderv1alpha1.CoderControlPlane) string {
	if coderControlPlane == nil {
		return ""
	}

	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(coderControlPlane.Namespace))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(coderControlPlane.Name))

	return fmt.Sprintf("%s-%016x", defaultOperatorAccessTokenName, hasher.Sum64())
}

func operatorAccessTokenSecretName(coderControlPlane *coderv1alpha1.CoderControlPlane) string {
	if coderControlPlane == nil {
		return ""
	}

	configuredSecretName := strings.TrimSpace(coderControlPlane.Spec.OperatorAccess.GeneratedTokenSecretName)
	if configuredSecretName != "" {
		return configuredSecretName
	}

	candidate := coderControlPlane.Name + operatorTokenSecretSuffix
	if len(candidate) <= 63 {
		return candidate
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(coderControlPlane.Name))
	hashSuffix := fmt.Sprintf("%08x", hasher.Sum32())
	available := 63 - len(operatorTokenSecretSuffix) - len(hashSuffix) - 1
	if available < 1 {
		available = 1
	}

	return fmt.Sprintf("%s-%s%s", coderControlPlane.Name[:available], hashSuffix, operatorTokenSecretSuffix)
}

func volumeNameForSecret(prefix, secretName string) string {
	normalizedSecretName := strings.TrimSpace(strings.ToLower(secretName))
	sanitizedSecretName := sanitizeDNSLabel(normalizedSecretName)
	candidate := fmt.Sprintf("%s-%s", prefix, sanitizedSecretName)
	if len(candidate) <= 63 && sanitizedSecretName == normalizedSecretName {
		return candidate
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(prefix))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(secretName))
	hashSuffix := fmt.Sprintf("%08x", hasher.Sum32())

	available := 63 - len(prefix) - len(hashSuffix) - 2
	if available < 1 {
		available = 1
	}
	if len(sanitizedSecretName) > available {
		sanitizedSecretName = sanitizedSecretName[:available]
		sanitizedSecretName = strings.Trim(sanitizedSecretName, "-")
		if sanitizedSecretName == "" {
			sanitizedSecretName = "x"
		}
	}

	return fmt.Sprintf("%s-%s-%s", prefix, sanitizedSecretName, hashSuffix)
}

func sanitizeDNSLabel(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "x"
	}

	builder := strings.Builder{}
	builder.Grow(len(value))
	lastWasDash := false
	for i := 0; i < len(value); i++ {
		char := value[i]
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteByte(char)
			lastWasDash = false
			continue
		}
		if !lastWasDash {
			builder.WriteByte('-')
			lastWasDash = true
		}
	}

	sanitized := strings.Trim(builder.String(), "-")
	if sanitized == "" {
		return "x"
	}

	return sanitized
}

func (r *CoderControlPlaneReconciler) ensureOperatorTokenSecret(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	name string,
	key string,
	token string,
) error {
	if coderControlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("assertion failed: secret name must not be empty")
	}
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("assertion failed: secret key must not be empty")
	}
	if token == "" {
		return fmt.Errorf("assertion failed: secret token must not be empty")
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: coderControlPlane.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = maps.Clone(controlPlaneLabels(coderControlPlane.Name))
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data[key] = []byte(token)

		if err := controllerutil.SetControllerReference(coderControlPlane, secret, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("reconcile operator token secret %q: %w", name, err)
	}

	return nil
}

func (r *CoderControlPlaneReconciler) readSecretValue(ctx context.Context, namespace, name, key string) (string, error) {
	if strings.TrimSpace(namespace) == "" {
		return "", fmt.Errorf("assertion failed: secret namespace must not be empty")
	}
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("assertion failed: secret name must not be empty")
	}
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("assertion failed: secret key must not be empty")
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
		return "", err
	}

	value, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("%w: secret %q does not contain key %q", errSecretValueMissing, name, key)
	}
	if len(value) == 0 {
		return "", fmt.Errorf("%w: secret %q key %q is empty", errSecretValueEmpty, name, key)
	}

	return string(value), nil
}

func setControlPlaneCondition(
	nextStatus *coderv1alpha1.CoderControlPlaneStatus,
	generation int64,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	if nextStatus == nil {
		return fmt.Errorf("assertion failed: next status must not be nil")
	}
	if strings.TrimSpace(conditionType) == "" {
		return fmt.Errorf("assertion failed: condition type must not be empty")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("assertion failed: condition reason must not be empty")
	}

	meta.SetStatusCondition(&nextStatus.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: generation,
		Reason:             reason,
		Message:            message,
	})

	return nil
}

func hashLicenseJWT(licenseJWT string) (string, error) {
	if licenseJWT == "" {
		return "", fmt.Errorf("assertion failed: license JWT must not be empty")
	}

	sum := sha256.Sum256([]byte(licenseJWT))
	return hex.EncodeToString(sum[:]), nil
}

func mergeResults(results ...ctrl.Result) ctrl.Result {
	merged := ctrl.Result{}
	for _, result := range results {
		if result.RequeueAfter > 0 && (merged.RequeueAfter == 0 || result.RequeueAfter < merged.RequeueAfter) {
			merged.RequeueAfter = result.RequeueAfter
		}
	}

	return merged
}

func indexByLicenseSecretName(obj client.Object) []string {
	coderControlPlane, ok := obj.(*coderv1alpha1.CoderControlPlane)
	if !ok || coderControlPlane.Spec.LicenseSecretRef == nil {
		return nil
	}

	licenseSecretName := strings.TrimSpace(coderControlPlane.Spec.LicenseSecretRef.Name)
	if licenseSecretName == "" {
		return nil
	}

	return []string{licenseSecretName}
}

func (r *CoderControlPlaneReconciler) reconcileRequestsForLicenseSecret(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	if strings.TrimSpace(secret.Name) == "" || strings.TrimSpace(secret.Namespace) == "" {
		return nil
	}

	var coderControlPlanes coderv1alpha1.CoderControlPlaneList
	if err := r.List(
		ctx,
		&coderControlPlanes,
		client.InNamespace(secret.Namespace),
		client.MatchingFields{licenseSecretNameFieldIndex: secret.Name},
	); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(coderControlPlanes.Items))
	for _, coderControlPlane := range coderControlPlanes.Items {
		if strings.TrimSpace(coderControlPlane.Name) == "" || strings.TrimSpace(coderControlPlane.Namespace) == "" {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      coderControlPlane.Name,
			Namespace: coderControlPlane.Namespace,
		}})
	}

	return requests
}

func isDuplicateLicenseUploadError(err error) bool {
	var sdkErr *codersdk.Error
	if !errors.As(err, &sdkErr) {
		return false
	}

	message := strings.ToLower(strings.TrimSpace(sdkErr.Message + " " + sdkErr.Detail))
	if message == "" {
		return false
	}

	if strings.Contains(message, "licenses_jwt_key") {
		return true
	}
	if strings.Contains(message, "duplicate key") {
		return true
	}
	if strings.Contains(message, "already exists") {
		return true
	}

	return false
}

func mergeControlPlaneStatusDelta(
	baseStatus coderv1alpha1.CoderControlPlaneStatus,
	nextStatus coderv1alpha1.CoderControlPlaneStatus,
	latestStatus coderv1alpha1.CoderControlPlaneStatus,
) coderv1alpha1.CoderControlPlaneStatus {
	mergedStatus := latestStatus

	if baseStatus.ObservedGeneration != nextStatus.ObservedGeneration {
		mergedStatus.ObservedGeneration = nextStatus.ObservedGeneration
	}
	if baseStatus.ReadyReplicas != nextStatus.ReadyReplicas {
		mergedStatus.ReadyReplicas = nextStatus.ReadyReplicas
	}
	if baseStatus.URL != nextStatus.URL {
		mergedStatus.URL = nextStatus.URL
	}
	if !equality.Semantic.DeepEqual(baseStatus.OperatorTokenSecretRef, nextStatus.OperatorTokenSecretRef) {
		mergedStatus.OperatorTokenSecretRef = cloneSecretKeySelector(nextStatus.OperatorTokenSecretRef)
	}
	if baseStatus.OperatorAccessReady != nextStatus.OperatorAccessReady {
		mergedStatus.OperatorAccessReady = nextStatus.OperatorAccessReady
	}
	if !equality.Semantic.DeepEqual(baseStatus.LicenseLastApplied, nextStatus.LicenseLastApplied) {
		mergedStatus.LicenseLastApplied = cloneMetav1Time(nextStatus.LicenseLastApplied)
	}
	if baseStatus.LicenseLastAppliedHash != nextStatus.LicenseLastAppliedHash {
		mergedStatus.LicenseLastAppliedHash = nextStatus.LicenseLastAppliedHash
	}
	if baseStatus.LicenseTier != nextStatus.LicenseTier {
		mergedStatus.LicenseTier = nextStatus.LicenseTier
	}
	if !equality.Semantic.DeepEqual(baseStatus.EntitlementsLastChecked, nextStatus.EntitlementsLastChecked) {
		mergedStatus.EntitlementsLastChecked = cloneMetav1Time(nextStatus.EntitlementsLastChecked)
	}
	if baseStatus.ExternalProvisionerDaemonsEntitlement != nextStatus.ExternalProvisionerDaemonsEntitlement {
		mergedStatus.ExternalProvisionerDaemonsEntitlement = nextStatus.ExternalProvisionerDaemonsEntitlement
	}
	if baseStatus.Phase != nextStatus.Phase {
		mergedStatus.Phase = nextStatus.Phase
	}
	if !equality.Semantic.DeepEqual(baseStatus.Conditions, nextStatus.Conditions) {
		mergedStatus.Conditions = append([]metav1.Condition(nil), nextStatus.Conditions...)
	}

	return mergedStatus
}

func cloneSecretKeySelector(selector *coderv1alpha1.SecretKeySelector) *coderv1alpha1.SecretKeySelector {
	if selector == nil {
		return nil
	}

	copied := *selector
	return &copied
}

func cloneMetav1Time(timestamp *metav1.Time) *metav1.Time {
	if timestamp == nil {
		return nil
	}

	return timestamp.DeepCopy()
}

func (r *CoderControlPlaneReconciler) reconcileStatus(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	baseStatus coderv1alpha1.CoderControlPlaneStatus,
	nextStatus coderv1alpha1.CoderControlPlaneStatus,
) error {
	if coderControlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	if equality.Semantic.DeepEqual(baseStatus, nextStatus) {
		return nil
	}

	namespacedName := types.NamespacedName{Name: coderControlPlane.Name, Namespace: coderControlPlane.Namespace}
	if strings.TrimSpace(namespacedName.Name) == "" || strings.TrimSpace(namespacedName.Namespace) == "" {
		return fmt.Errorf("assertion failed: coder control plane namespaced name must not be empty")
	}

	statusReader := r.APIReader
	if statusReader == nil {
		statusReader = r.Client
	}
	if statusReader == nil {
		return fmt.Errorf("assertion failed: status reader must not be nil")
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &coderv1alpha1.CoderControlPlane{}
		if err := statusReader.Get(ctx, namespacedName, latest); err != nil {
			return err
		}
		if latest.Name != namespacedName.Name || latest.Namespace != namespacedName.Namespace {
			return fmt.Errorf("assertion failed: fetched object %s/%s does not match expected %s/%s",
				latest.Namespace, latest.Name, namespacedName.Namespace, namespacedName.Name)
		}
		if nextStatus.ObservedGeneration > 0 && latest.Generation != nextStatus.ObservedGeneration {
			// A newer reconcile has observed a newer generation. Avoid overwriting
			// status with stale data from an older reconcile attempt.
			coderControlPlane.Status = latest.Status
			return nil
		}

		mergedStatus := mergeControlPlaneStatusDelta(baseStatus, nextStatus, latest.Status)
		if equality.Semantic.DeepEqual(latest.Status, mergedStatus) {
			coderControlPlane.Status = latest.Status
			return nil
		}

		latest.Status = mergedStatus
		if err := r.Status().Update(ctx, latest); err != nil {
			return err
		}

		coderControlPlane.Status = mergedStatus
		return nil
	}); err != nil {
		return fmt.Errorf("update control plane status: %w", err)
	}

	return nil
}

// SetupWithManager wires the reconciler into controller-runtime.
func (r *CoderControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil {
		return fmt.Errorf("assertion failed: manager must not be nil")
	}
	if r.Client == nil {
		return fmt.Errorf("assertion failed: reconciler client must not be nil")
	}
	if r.Scheme == nil {
		return fmt.Errorf("assertion failed: reconciler scheme must not be nil")
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&coderv1alpha1.CoderControlPlane{},
		licenseSecretNameFieldIndex,
		indexByLicenseSecretName,
	); err != nil {
		return fmt.Errorf("index coder control planes by license secret name: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&coderv1alpha1.CoderControlPlane{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&corev1.Secret{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.reconcileRequestsForLicenseSecret),
		).
		Named("codercontrolplane").
		Complete(r)
}

func controlPlaneLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "coder-control-plane",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/managed-by": "coder-k8s",
	}
}
