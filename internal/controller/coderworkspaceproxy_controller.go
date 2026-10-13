// Package controller contains Kubernetes controllers for coder-k8s resources.
package controller

import (
	"context"
	"fmt"
	"hash/fnv"
	"maps"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// workspaceProxyReservedSettings are the env names and flags that
// reconcileDeployment sets. Source (github.com/coder/coder/v2 v2.37.3, the
// version in go.mod): enterprise/cli/proxyserver.go:69-70 (--proxy-session-token,
// CODER_PROXY_SESSION_TOKEN) and :81-82 (--primary-access-url,
// CODER_PRIMARY_ACCESS_URL).
var workspaceProxyReservedSettings = reservedSettings{
	envNames: []string{"CODER_PRIMARY_ACCESS_URL", "CODER_PROXY_SESSION_TOKEN"},
	flags:    []string{"primary-access-url", "proxy-session-token"},
}

const (
	defaultWorkspaceProxyPort = int32(80)
	workspaceProxyTargetPort  = int32(3001)
	workspaceProxyNamePrefix  = "wsproxy-"
)

// CoderWorkspaceProxyReconciler reconciles a CoderWorkspaceProxy object.
type CoderWorkspaceProxyReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	BootstrapClient coderbootstrap.Client
}

// +kubebuilder:rbac:groups=coder.com,resources=coderworkspaceproxies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coder.com,resources=coderworkspaceproxies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coder.com,resources=coderworkspaceproxies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile converges the desired CoderWorkspaceProxy spec into Deployment and Service resources.
func (r *CoderWorkspaceProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: reconciler client must not be nil")
	}
	if r.Scheme == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: reconciler scheme must not be nil")
	}

	workspaceProxy := &coderv1alpha1.CoderWorkspaceProxy{}
	if err := r.Get(ctx, req.NamespacedName, workspaceProxy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get coderworkspaceproxy %s: %w", req.NamespacedName, err)
	}

	if workspaceProxy.Name != req.Name || workspaceProxy.Namespace != req.Namespace {
		return ctrl.Result{}, fmt.Errorf("assertion failed: fetched object %s/%s does not match request %s/%s",
			workspaceProxy.Namespace, workspaceProxy.Name, req.Namespace, req.Name)
	}

	primaryAccessURL, tokenRef, registered, err := r.resolveProxyCredentials(ctx, workspaceProxy)
	if err != nil {
		// Fail static: a failed check sends nothing to Coder and leaves the
		// Deployment as it is. The Secret watch re-queues this proxy.
		if credErr, ok := asCredentialError(err); ok {
			return ctrl.Result{}, r.reportCredentialsNotReady(ctx, workspaceProxy, credErr)
		}
		return ctrl.Result{}, err
	}
	if primaryAccessURL == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: workspace proxy primary access URL must not be empty after credential checks")
	}
	if tokenRef == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: workspace proxy token reference must not be nil after credential checks")
	}

	deployment, err := r.reconcileDeployment(ctx, workspaceProxy, primaryAccessURL, tokenRef)
	if err != nil {
		return ctrl.Result{}, err
	}
	service, err := r.reconcileService(ctx, workspaceProxy)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileStatus(ctx, workspaceProxy, deployment, service, tokenRef, registered); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// resolveProxyCredentials checks every Secret input and setting of the proxy,
// then returns the primary access URL and the proxy token Secret to mount.
// Check failures are *credentialsecret.Error values. It sends the bootstrap
// request only after every check passed.
func (r *CoderWorkspaceProxyReconciler) resolveProxyCredentials(
	ctx context.Context,
	workspaceProxy *coderv1alpha1.CoderWorkspaceProxy,
) (string, *coderv1alpha1.SecretKeySelector, bool, error) {
	spec := workspaceProxy.Spec
	if err := checkReservedSettings(spec.ExtraEnv, spec.ExtraArgs, workspaceProxyReservedSettings); err != nil {
		return "", nil, false, err
	}

	primary := spec.PrimaryAccessURL
	if primary == "" && spec.Bootstrap != nil {
		primary = spec.Bootstrap.CoderURL
	}
	if primary == "" {
		return "", nil, false, &credentialsecret.Error{
			Reason:  credentialsecret.ReasonMalformed,
			Message: "Set spec.primaryAccessURL or spec.bootstrap.coderURL.",
		}
	}
	primaryURL, err := credentialsecret.ParseURL(primary)
	if err != nil {
		return "", nil, false, &credentialsecret.Error{
			Reason:  credentialsecret.ReasonMalformed,
			Message: fmt.Sprintf("The primary access URL is not valid: %v.", err),
		}
	}

	if err := checkExtraEnvSecrets(ctx, r.Client, workspaceProxy.Namespace, spec.ExtraEnv, primaryURL); err != nil {
		return "", nil, false, err
	}

	if spec.Bootstrap == nil {
		tokenRef := spec.ProxySessionTokenSecretRef
		if tokenRef == nil {
			return "", nil, false, &credentialsecret.Error{
				Reason:  credentialsecret.ReasonMalformed,
				Message: "Set spec.proxySessionTokenSecretRef or spec.bootstrap.",
			}
		}
		normalizedRef := *tokenRef
		if normalizedRef.Key == "" {
			normalizedRef.Key = coderv1alpha1.DefaultTokenSecretKey
		}
		if _, err := credentialsecret.Read(ctx, r.Client, workspaceProxy.Namespace, normalizedRef, primaryURL); err != nil {
			return "", nil, false, withCredentialField(err, "spec.proxySessionTokenSecretRef")
		}
		return primary, &normalizedRef, false, nil
	}

	if r.BootstrapClient == nil {
		return "", nil, false, fmt.Errorf("workspace proxy bootstrap is configured but bootstrap client is not available")
	}

	bootstrap := spec.Bootstrap
	coderURL, err := credentialsecret.ParseURL(bootstrap.CoderURL)
	if err != nil {
		return "", nil, false, &credentialsecret.Error{
			Reason:  credentialsecret.ReasonMalformed,
			Message: fmt.Sprintf("spec.bootstrap.coderURL is not valid: %v.", err),
		}
	}

	tokenSecretName := workspaceProxyTokenSecretName(workspaceProxy)
	proxyTokenKey := coderv1alpha1.DefaultTokenSecretKey
	tokenSecret, err := r.getOwnedTokenSecret(ctx, workspaceProxy, tokenSecretName)
	if err != nil {
		return "", nil, false, err
	}

	credentialsRef := bootstrap.CredentialsSecretRef
	if credentialsRef.Key == "" {
		credentialsRef.Key = coderv1alpha1.DefaultTokenSecretKey
	}
	credentials, err := credentialsecret.Read(ctx, r.Client, workspaceProxy.Namespace, credentialsRef, coderURL)
	if err != nil {
		return "", nil, false, withCredentialField(err, "spec.bootstrap.credentialsSecretRef")
	}
	// The generated proxy token comes from the bootstrap credentials, so the
	// pod's primary access URL must also be one of their destinations.
	if _, err := credentialsecret.Read(ctx, r.Client, workspaceProxy.Namespace, credentialsRef, primaryURL); err != nil {
		return "", nil, false, withCredentialField(err, "spec.bootstrap.credentialsSecretRef (proxy token destination)")
	}

	tokenRef := &coderv1alpha1.SecretKeySelector{Name: tokenSecretName, Key: proxyTokenKey}
	if tokenSecret != nil && len(tokenSecret.Data[proxyTokenKey]) > 0 {
		return primary, tokenRef, true, nil
	}

	// Every check passed. Record that before the Coder request, so a failed
	// request does not leave an outdated CredentialsReady=False behind.
	if err := r.writeCredentialsCondition(ctx, workspaceProxy, metav1.ConditionTrue, reasonCredentialsAccepted, credentialsAcceptedMessage); err != nil {
		return "", nil, false, err
	}

	proxyName := bootstrap.ProxyName
	if proxyName == "" {
		proxyName = workspaceProxy.Name
	}

	response, err := r.BootstrapClient.EnsureWorkspaceProxy(ctx, coderbootstrap.RegisterWorkspaceProxyRequest{
		CoderURL:     bootstrap.CoderURL,
		SessionToken: credentials.Reveal(),
		ProxyName:    proxyName,
		DisplayName:  bootstrap.DisplayName,
		Icon:         bootstrap.Icon,
	})
	if err != nil {
		return "", nil, false, fmt.Errorf("bootstrap workspace proxy registration: %s", credentialsecret.Redact(err.Error(), credentials))
	}

	if err := r.ensureTokenSecret(ctx, workspaceProxy, tokenSecret, tokenSecretName, proxyTokenKey, response.ProxyToken); err != nil {
		return "", nil, false, err
	}

	return primary, tokenRef, true, nil
}

// getOwnedTokenSecret returns the generated proxy token Secret, or nil when it
// does not exist. It returns a TokenSecretNotOwned error when the Secret
// exists and this proxy is not its controller: the operator then does not
// read, mount, write or adopt it.
func (r *CoderWorkspaceProxyReconciler) getOwnedTokenSecret(
	ctx context.Context,
	workspaceProxy *coderv1alpha1.CoderWorkspaceProxy,
	name string,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: workspaceProxy.Namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get proxy token secret %q: %w", name, err)
	}
	if isControlledBy(secret, workspaceProxy, "CoderWorkspaceProxy") {
		return secret, nil
	}

	message := fmt.Sprintf("Secret %q named in spec.bootstrap.generatedTokenSecretName exists and is not owned by this proxy. "+
		"The operator does not read or change it. Remove the field to use the default name, or name a Secret that does not exist yet.", name)
	if workspaceProxy.Spec.Bootstrap.GeneratedTokenSecretName == "" {
		message = fmt.Sprintf("Secret %q (the default generated token Secret) exists and is not owned by this proxy. "+
			"The operator does not read or change it. Delete that Secret, or set spec.bootstrap.generatedTokenSecretName to a name that does not exist yet.", name)
	}
	return nil, &credentialsecret.Error{Reason: reasonTokenSecretNotOwned, Message: message}
}

// reportCredentialsNotReady records a failed check in CredentialsReady and
// changes nothing else.
func (r *CoderWorkspaceProxyReconciler) reportCredentialsNotReady(
	ctx context.Context,
	workspaceProxy *coderv1alpha1.CoderWorkspaceProxy,
	credErr *credentialsecret.Error,
) error {
	if credErr == nil {
		return fmt.Errorf("assertion failed: credential error must not be nil")
	}
	ctrl.LoggerFrom(ctx).Info("workspace proxy credential check failed; keeping the last applied configuration",
		"reason", credErr.Reason, "message", credErr.Message)
	return r.writeCredentialsCondition(ctx, workspaceProxy, metav1.ConditionFalse, credErr.Reason,
		credErr.Message+" The proxy keeps its last applied configuration.")
}

// writeCredentialsCondition sets CredentialsReady and writes status only when
// the condition changes.
func (r *CoderWorkspaceProxyReconciler) writeCredentialsCondition(
	ctx context.Context,
	workspaceProxy *coderv1alpha1.CoderWorkspaceProxy,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	conditions := slices.Clone(workspaceProxy.Status.Conditions)
	meta.SetStatusCondition(&conditions, metav1.Condition{
		Type:               coderv1alpha1.CoderWorkspaceProxyConditionCredentialsReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: workspaceProxy.Generation,
	})
	if equality.Semantic.DeepEqual(workspaceProxy.Status.Conditions, conditions) {
		return nil
	}
	workspaceProxy.Status.Conditions = conditions
	if err := r.Status().Update(ctx, workspaceProxy); err != nil {
		return fmt.Errorf("update workspace proxy credentials condition: %w", err)
	}
	return nil
}

func (r *CoderWorkspaceProxyReconciler) reconcileDeployment(
	ctx context.Context,
	workspaceProxy *coderv1alpha1.CoderWorkspaceProxy,
	primaryAccessURL string,
	tokenRef *coderv1alpha1.SecretKeySelector,
) (*appsv1.Deployment, error) {
	deploymentName := workspaceProxyResourceName(workspaceProxy.Name)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: workspaceProxy.Namespace}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		labels := workspaceProxyLabels(workspaceProxy.Name)
		deployment.Labels = maps.Clone(labels)

		if err := controllerutil.SetControllerReference(workspaceProxy, deployment, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		replicas := int32(1)
		if workspaceProxy.Spec.Replicas != nil {
			replicas = *workspaceProxy.Spec.Replicas
		}

		image := workspaceProxy.Spec.Image
		if image == "" {
			image = defaultCoderImage
		}

		args := []string{"wsproxy", "server", "--http-address=0.0.0.0:3001"}
		if workspaceProxy.Spec.DerpOnly {
			args = append(args, "--derp-only")
		}
		args = append(args, workspaceProxy.Spec.ExtraArgs...)

		env := []corev1.EnvVar{
			{Name: "CODER_PRIMARY_ACCESS_URL", Value: primaryAccessURL},
			{Name: "CODER_PROXY_SESSION_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: tokenRef.Name}, Key: tokenRef.Key}}},
		}
		env = append(env, workspaceProxy.Spec.ExtraEnv...)

		deployment.Spec.Replicas = &replicas
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: maps.Clone(labels)}
		deployment.Spec.Template = withPreservedRolloutRestart(deployment.Spec.Template, corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: maps.Clone(labels)},
			Spec: corev1.PodSpec{
				// The proxy talks only to coderd and needs no Kubernetes API token.
				AutomountServiceAccountToken: ptr.To(false),
				ImagePullSecrets:             workspaceProxy.Spec.ImagePullSecrets,
				Containers: []corev1.Container{{
					Name:  "workspace-proxy",
					Image: image,
					Args:  args,
					Env:   env,
					Ports: []corev1.ContainerPort{{
						Name:          "http",
						ContainerPort: workspaceProxyTargetPort,
						Protocol:      corev1.ProtocolTCP,
					}},
				}},
			},
		})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile workspace proxy deployment: %w", err)
	}

	// Avoid an immediate cached read-after-write here; cache propagation lag can
	// transiently return NotFound for just-created objects and produce noisy reconcile errors.
	return deployment, nil
}

func (r *CoderWorkspaceProxyReconciler) reconcileService(ctx context.Context, workspaceProxy *coderv1alpha1.CoderWorkspaceProxy) (*corev1.Service, error) {
	serviceName := workspaceProxyResourceName(workspaceProxy.Name)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: workspaceProxy.Namespace}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		labels := workspaceProxyLabels(workspaceProxy.Name)
		service.Labels = maps.Clone(labels)
		service.Annotations = maps.Clone(workspaceProxy.Spec.Service.Annotations)

		if err := controllerutil.SetControllerReference(workspaceProxy, service, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		serviceType := workspaceProxy.Spec.Service.Type
		if serviceType == "" {
			serviceType = corev1.ServiceTypeClusterIP
		}
		servicePort := workspaceProxy.Spec.Service.Port
		if servicePort == 0 {
			servicePort = defaultWorkspaceProxyPort
		}

		service.Spec.Type = serviceType
		service.Spec.Selector = maps.Clone(labels)
		service.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       servicePort,
			Protocol:   corev1.ProtocolTCP,
			TargetPort: intstr.FromInt(int(workspaceProxyTargetPort)),
		}}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile workspace proxy service: %w", err)
	}

	// Avoid an immediate cached read-after-write here; cache propagation lag can
	// transiently return NotFound for just-created objects and produce noisy reconcile errors.
	return service, nil
}

func (r *CoderWorkspaceProxyReconciler) reconcileStatus(
	ctx context.Context,
	workspaceProxy *coderv1alpha1.CoderWorkspaceProxy,
	deployment *appsv1.Deployment,
	_ *corev1.Service,
	tokenRef *coderv1alpha1.SecretKeySelector,
	registered bool,
) error {
	phase := coderv1alpha1.WorkspaceProxyPhasePending
	if deployment.Status.ReadyReplicas > 0 {
		phase = coderv1alpha1.WorkspaceProxyPhaseReady
	}

	nextStatus := coderv1alpha1.WorkspaceProxyStatus{
		ObservedGeneration: workspaceProxy.Generation,
		ReadyReplicas:      deployment.Status.ReadyReplicas,
		Registered:         registered,
		ProxyTokenSecretRef: &coderv1alpha1.SecretKeySelector{
			Name: tokenRef.Name,
			Key:  tokenRef.Key,
		},
		Phase:      phase,
		Conditions: slices.Clone(workspaceProxy.Status.Conditions),
	}
	meta.SetStatusCondition(&nextStatus.Conditions, metav1.Condition{
		Type:               coderv1alpha1.CoderWorkspaceProxyConditionCredentialsReady,
		Status:             metav1.ConditionTrue,
		Reason:             reasonCredentialsAccepted,
		Message:            credentialsAcceptedMessage,
		ObservedGeneration: workspaceProxy.Generation,
	})
	if equality.Semantic.DeepEqual(workspaceProxy.Status, nextStatus) {
		return nil
	}

	workspaceProxy.Status = nextStatus
	if err := r.Status().Update(ctx, workspaceProxy); err != nil {
		return fmt.Errorf("update workspace proxy status: %w", err)
	}

	return nil
}

// ensureTokenSecret creates the generated token Secret when existing is nil,
// or updates existing when this proxy controls it. It never adopts a Secret.
func (r *CoderWorkspaceProxyReconciler) ensureTokenSecret(
	ctx context.Context,
	workspaceProxy *coderv1alpha1.CoderWorkspaceProxy,
	existing *corev1.Secret,
	name string,
	key string,
	token string,
) error {
	if token == "" {
		return fmt.Errorf("assertion failed: generated proxy token must not be empty")
	}
	labels := workspaceProxyLabels(workspaceProxy.Name)

	if existing == nil {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workspaceProxy.Namespace, Labels: maps.Clone(labels)},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{key: []byte(token)},
		}
		if err := controllerutil.SetControllerReference(workspaceProxy, secret, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference on proxy token secret %q: %w", name, err)
		}
		if err := r.Create(ctx, secret); err != nil {
			return fmt.Errorf("create proxy token secret %q: %w", name, err)
		}
		return nil
	}

	if existing.Name != name || existing.Namespace != workspaceProxy.Namespace {
		return fmt.Errorf("assertion failed: proxy token secret %s/%s does not match %s/%s", existing.Namespace, existing.Name, workspaceProxy.Namespace, name)
	}
	if !isControlledBy(existing, workspaceProxy, "CoderWorkspaceProxy") {
		return fmt.Errorf("assertion failed: proxy token secret %q must be controlled by this proxy before an update", name)
	}
	updated := existing.DeepCopy()
	updated.Labels = maps.Clone(labels)
	if updated.Data == nil {
		updated.Data = make(map[string][]byte)
	}
	updated.Data[key] = []byte(token)
	if equality.Semantic.DeepEqual(existing, updated) {
		return nil
	}
	if err := r.Update(ctx, updated); err != nil {
		return fmt.Errorf("update proxy token secret %q: %w", name, err)
	}
	return nil
}

// SetupWithManager wires the reconciler into controller-runtime.
func (r *CoderWorkspaceProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
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
		&coderv1alpha1.CoderWorkspaceProxy{},
		credentialSecretNamesFieldIndex,
		indexWorkspaceProxyBySecretNames,
	); err != nil {
		return fmt.Errorf("index coder workspace proxies by secret names: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&coderv1alpha1.CoderWorkspaceProxy{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.reconcileRequestsForSecret),
			builder.WithPredicates(credentialsecret.WatchPredicate()),
		).
		Watches(
			&coderv1alpha1.CoderControlPlane{},
			handler.EnqueueRequestsFromMapFunc(r.reconcileRequestsForControlPlane),
		).
		Named("coderworkspaceproxy").
		Complete(r)
}

// indexWorkspaceProxyBySecretNames returns every Secret name the proxy reads
// or mounts, including the generated token Secret.
func indexWorkspaceProxyBySecretNames(obj client.Object) []string {
	workspaceProxy, ok := obj.(*coderv1alpha1.CoderWorkspaceProxy)
	if !ok {
		return nil
	}

	names := extraEnvSecretNames(workspaceProxy.Spec.ExtraEnv)
	if ref := workspaceProxy.Spec.ProxySessionTokenSecretRef; ref != nil {
		names = append(names, ref.Name)
	}
	if workspaceProxy.Spec.Bootstrap != nil {
		names = append(names, workspaceProxy.Spec.Bootstrap.CredentialsSecretRef.Name, workspaceProxyTokenSecretName(workspaceProxy))
	}
	return sortedUnique(names)
}

func (r *CoderWorkspaceProxyReconciler) reconcileRequestsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret.Name == "" || secret.Namespace == "" {
		return nil
	}

	var workspaceProxies coderv1alpha1.CoderWorkspaceProxyList
	if err := r.List(ctx, &workspaceProxies,
		client.InNamespace(secret.Namespace),
		client.MatchingFields{credentialSecretNamesFieldIndex: secret.Name},
	); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "list workspace proxies for secret", "secret", secret.Name, "namespace", secret.Namespace)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(workspaceProxies.Items))
	for _, workspaceProxy := range workspaceProxies.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}})
	}
	return requests
}

// reconcileRequestsForControlPlane re-queues the proxies that reference a
// Secret that the control plane uses for its own access, because
// credentialsecret.Read refuses those. On an update, controller-runtime maps
// the old and the new object, so a name that the control plane stops using
// re-queues its proxies too.
func (r *CoderWorkspaceProxyReconciler) reconcileRequestsForControlPlane(ctx context.Context, obj client.Object) []reconcile.Request {
	controlPlane, ok := obj.(*coderv1alpha1.CoderControlPlane)
	if !ok || controlPlane.Namespace == "" {
		return nil
	}
	var requests []reconcile.Request
	for _, name := range credentialsecret.ControlPlaneSecretNames(controlPlane) {
		requests = append(requests, r.reconcileRequestsForSecret(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controlPlane.Namespace}})...)
	}
	return requests
}

func workspaceProxyTokenSecretName(workspaceProxy *coderv1alpha1.CoderWorkspaceProxy) string {
	if workspaceProxy.Spec.Bootstrap != nil && workspaceProxy.Spec.Bootstrap.GeneratedTokenSecretName != "" {
		return workspaceProxy.Spec.Bootstrap.GeneratedTokenSecretName
	}
	return fmt.Sprintf("%s-proxy-token", workspaceProxy.Name)
}

func workspaceProxyResourceName(name string) string {
	candidate := workspaceProxyNamePrefix + name
	if len(candidate) <= 63 {
		return candidate
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(name))
	suffix := fmt.Sprintf("%08x", hasher.Sum32())
	available := 63 - len(workspaceProxyNamePrefix) - len(suffix) - 1
	if available < 1 {
		available = 1
	}

	return fmt.Sprintf("%s%s-%s", workspaceProxyNamePrefix, name[:available], suffix)
}

func workspaceProxyLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "coder-workspace-proxy",
		"app.kubernetes.io/instance":   workspaceProxyInstanceLabelValue(name),
		"app.kubernetes.io/managed-by": "coder-k8s",
	}
}

func workspaceProxyInstanceLabelValue(name string) string {
	if len(name) <= 63 {
		return name
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(name))
	suffix := fmt.Sprintf("%08x", hasher.Sum32())
	available := 63 - len(suffix) - 1
	if available < 1 {
		available = 1
	}

	return fmt.Sprintf("%s-%s", name[:available], suffix)
}
