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
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
)

const (
	defaultCoderImage       = "ghcr.io/coder/coder:latest"
	defaultControlPlanePort = int32(80)
	controlPlaneTargetPort  = int32(3000)

	postgresConnectionURLEnvVar = "CODER_PG_CONNECTION_URL"

	defaultOperatorAccessUsername = "coder-k8s-operator"
	defaultOperatorAccessEmail    = "coder-k8s-operator@coder-k8s.invalid"
	// #nosec G101 -- this is a static token label used as a database identifier.
	defaultOperatorAccessTokenName     = "coder-k8s-operator"
	defaultOperatorAccessTokenLifetime = 365 * 24 * time.Hour

	operatorAccessRetryInterval = 30 * time.Second
	operatorTokenSecretSuffix   = "-operator-token"

	// #nosec G101 -- this is a field index key, not a credential.
	licenseSecretNameFieldIndex = ".spec.licenseSecretRef.name"

	licenseConditionReasonApplied       = "Applied"
	licenseConditionReasonPending       = "Pending"
	licenseConditionReasonSecretMissing = "SecretMissing"
	licenseConditionReasonForbidden     = "Forbidden"
	licenseConditionReasonNotSupported  = "NotSupported"
	licenseConditionReasonError         = "Error"

	licenseUploadRequestTimeout = 30 * time.Second
)

var (
	errSecretValueMissing = errors.New("secret value missing")
	errSecretValueEmpty   = errors.New("secret value empty")
)

// LicenseUploader uploads Coder Enterprise license JWTs to a coderd instance.
type LicenseUploader interface {
	AddLicense(ctx context.Context, coderURL, sessionToken, licenseJWT string) error
}

// NewSDKLicenseUploader returns a LicenseUploader backed by codersdk.
func NewSDKLicenseUploader() LicenseUploader {
	return &sdkLicenseUploader{}
}

type sdkLicenseUploader struct{}

func (u *sdkLicenseUploader) AddLicense(ctx context.Context, coderURL, sessionToken, licenseJWT string) error {
	if strings.TrimSpace(coderURL) == "" {
		return fmt.Errorf("assertion failed: coder URL must not be empty")
	}
	if sessionToken == "" {
		return fmt.Errorf("assertion failed: session token must not be empty")
	}
	if licenseJWT == "" {
		return fmt.Errorf("assertion failed: license JWT must not be empty")
	}

	parsedURL, err := url.Parse(coderURL)
	if err != nil {
		return fmt.Errorf("parse coder URL: %w", err)
	}

	sdkClient := codersdk.New(parsedURL)
	sdkClient.SetSessionToken(sessionToken)
	if sdkClient.HTTPClient == nil {
		sdkClient.HTTPClient = &http.Client{}
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return fmt.Errorf("assertion failed: http.DefaultTransport is not *http.Transport")
	}
	// Use a dedicated transport to avoid sharing http.DefaultTransport's
	// connection pool across parallel test servers.
	sdkClient.HTTPClient.Transport = defaultTransport.Clone()
	sdkClient.HTTPClient.Timeout = licenseUploadRequestTimeout

	if _, err := sdkClient.AddLicense(ctx, codersdk.AddLicenseRequest{License: licenseJWT}); err != nil {
		return fmt.Errorf("upload coder license: %w", err)
	}

	return nil
}

// CoderControlPlaneReconciler reconciles a CoderControlPlane object.
type CoderControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	OperatorAccessProvisioner coderbootstrap.OperatorAccessProvisioner
	LicenseUploader           LicenseUploader
}

// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

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

	deployment, err := r.reconcileDeployment(ctx, coderControlPlane)
	if err != nil {
		return ctrl.Result{}, err
	}
	service, err := r.reconcileService(ctx, coderControlPlane)
	if err != nil {
		return ctrl.Result{}, err
	}

	nextStatus := r.desiredStatus(coderControlPlane, deployment, service)

	operatorResult, err := r.reconcileOperatorAccess(ctx, coderControlPlane, &nextStatus)
	if err != nil {
		return ctrl.Result{}, err
	}

	licenseResult, err := r.reconcileLicense(ctx, coderControlPlane, &nextStatus)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileStatus(ctx, coderControlPlane, nextStatus); err != nil {
		return ctrl.Result{}, err
	}

	return mergeResults(operatorResult, licenseResult), nil
}

func (r *CoderControlPlaneReconciler) reconcileDeployment(ctx context.Context, coderControlPlane *coderv1alpha1.CoderControlPlane) (*appsv1.Deployment, error) {
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

		args := []string{"--http-address=0.0.0.0:3000"}
		args = append(args, coderControlPlane.Spec.ExtraArgs...)

		deployment.Spec.Replicas = &replicas
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: maps.Clone(labels)}
		deployment.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: maps.Clone(labels)},
			Spec: corev1.PodSpec{
				ImagePullSecrets: coderControlPlane.Spec.ImagePullSecrets,
				Containers: []corev1.Container{{
					Name:  "coder",
					Image: image,
					Args:  args,
					Env:   coderControlPlane.Spec.ExtraEnv,
					Ports: []corev1.ContainerPort{{
						Name:          "http",
						ContainerPort: controlPlaneTargetPort,
						Protocol:      corev1.ProtocolTCP,
					}},
				}},
			},
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

		service.Spec.Type = serviceType
		service.Spec.Selector = maps.Clone(labels)
		service.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       servicePort,
			Protocol:   corev1.ProtocolTCP,
			TargetPort: intstr.FromInt(int(controlPlaneTargetPort)),
		}}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile control plane service: %w", err)
	}

	// Avoid an immediate cached read-after-write here; cache propagation lag can
	// transiently return NotFound for just-created objects and produce noisy reconcile errors.
	return service, nil
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

	nextStatus.ObservedGeneration = coderControlPlane.Generation
	nextStatus.ReadyReplicas = deployment.Status.ReadyReplicas
	nextStatus.URL = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", service.Name, service.Namespace, servicePort)
	nextStatus.Phase = phase

	return nextStatus
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

	if strings.TrimSpace(nextStatus.URL) == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: control plane URL must not be empty when licenseSecretRef is configured")
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

	if err := r.LicenseUploader.AddLicense(ctx, nextStatus.URL, operatorToken, licenseJWT); err != nil {
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
	if secret == nil || coderControlPlane == nil {
		return false
	}

	ownerReference := metav1.GetControllerOf(secret)
	if ownerReference == nil {
		return false
	}

	return ownerReference.APIVersion == coderv1alpha1.GroupVersion.String() &&
		ownerReference.Kind == "CoderControlPlane" &&
		ownerReference.Name == coderControlPlane.Name &&
		ownerReference.UID == coderControlPlane.UID
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

func (r *CoderControlPlaneReconciler) reconcileStatus(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	nextStatus coderv1alpha1.CoderControlPlaneStatus,
) error {
	if equality.Semantic.DeepEqual(coderControlPlane.Status, nextStatus) {
		return nil
	}

	coderControlPlane.Status = nextStatus
	if err := r.Status().Update(ctx, coderControlPlane); err != nil {
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
