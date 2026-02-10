// Package controller contains Kubernetes controllers for coder-k8s resources.
package controller

import (
	"context"
	"fmt"
	"hash/fnv"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
)

const (
	defaultWorkspaceProxyPort = int32(80)
	workspaceProxyTargetPort  = int32(3001)
	workspaceProxyNamePrefix  = "wsproxy-"
)

// WorkspaceProxyReconciler reconciles a WorkspaceProxy object.
type WorkspaceProxyReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	BootstrapClient coderbootstrap.Client
}

// +kubebuilder:rbac:groups=coder.com,resources=workspaceproxies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coder.com,resources=workspaceproxies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coder.com,resources=workspaceproxies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile converges the desired WorkspaceProxy spec into Deployment and Service resources.
func (r *WorkspaceProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: reconciler client must not be nil")
	}
	if r.Scheme == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: reconciler scheme must not be nil")
	}

	workspaceProxy := &coderv1alpha1.WorkspaceProxy{}
	if err := r.Get(ctx, req.NamespacedName, workspaceProxy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get workspaceproxy %s: %w", req.NamespacedName, err)
	}

	if workspaceProxy.Name != req.Name || workspaceProxy.Namespace != req.Namespace {
		return ctrl.Result{}, fmt.Errorf("assertion failed: fetched object %s/%s does not match request %s/%s",
			workspaceProxy.Namespace, workspaceProxy.Name, req.Namespace, req.Name)
	}

	primaryAccessURL, tokenRef, registered, err := r.resolveProxyCredentials(ctx, workspaceProxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if primaryAccessURL == "" {
		return ctrl.Result{}, fmt.Errorf("workspace proxy primaryAccessURL must be provided directly or via bootstrap.coderURL")
	}
	if tokenRef == nil {
		return ctrl.Result{}, fmt.Errorf("workspace proxy session token reference must be provided directly or via bootstrap")
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

func (r *WorkspaceProxyReconciler) resolveProxyCredentials(
	ctx context.Context,
	workspaceProxy *coderv1alpha1.WorkspaceProxy,
) (string, *coderv1alpha1.SecretKeySelector, bool, error) {
	if workspaceProxy.Spec.Bootstrap == nil {
		tokenRef := workspaceProxy.Spec.ProxySessionTokenSecretRef
		if tokenRef != nil && tokenRef.Key == "" {
			normalizedRef := *tokenRef
			normalizedRef.Key = coderv1alpha1.DefaultTokenSecretKey
			tokenRef = &normalizedRef
		}
		return workspaceProxy.Spec.PrimaryAccessURL, tokenRef, false, nil
	}

	if r.BootstrapClient == nil {
		return "", nil, false, fmt.Errorf("workspace proxy bootstrap is configured but bootstrap client is not available")
	}

	bootstrap := workspaceProxy.Spec.Bootstrap
	tokenSecretName := bootstrap.GeneratedTokenSecretName
	if tokenSecretName == "" {
		tokenSecretName = fmt.Sprintf("%s-proxy-token", workspaceProxy.Name)
	}
	proxyTokenKey := coderv1alpha1.DefaultTokenSecretKey
	if existingToken, readErr := r.readSecretValue(ctx, workspaceProxy.Namespace, tokenSecretName, proxyTokenKey); readErr == nil && existingToken != "" {
		primary := workspaceProxy.Spec.PrimaryAccessURL
		if primary == "" {
			primary = bootstrap.CoderURL
		}
		return primary, &coderv1alpha1.SecretKeySelector{Name: tokenSecretName, Key: proxyTokenKey}, true, nil
	}

	secretKey := bootstrap.CredentialsSecretRef.Key
	if secretKey == "" {
		secretKey = coderv1alpha1.DefaultTokenSecretKey
	}

	credentials, err := r.readSecretValue(ctx, workspaceProxy.Namespace, bootstrap.CredentialsSecretRef.Name, secretKey)
	if err != nil {
		return "", nil, false, fmt.Errorf("read bootstrap credentials: %w", err)
	}

	proxyName := bootstrap.ProxyName
	if proxyName == "" {
		proxyName = workspaceProxy.Name
	}

	response, err := r.BootstrapClient.EnsureWorkspaceProxy(ctx, coderbootstrap.RegisterWorkspaceProxyRequest{
		CoderURL:     bootstrap.CoderURL,
		SessionToken: credentials,
		ProxyName:    proxyName,
		DisplayName:  bootstrap.DisplayName,
		Icon:         bootstrap.Icon,
	})
	if err != nil {
		return "", nil, false, fmt.Errorf("bootstrap workspace proxy registration: %w", err)
	}

	if err := r.ensureTokenSecret(ctx, workspaceProxy, tokenSecretName, proxyTokenKey, response.ProxyToken); err != nil {
		return "", nil, false, err
	}

	primary := workspaceProxy.Spec.PrimaryAccessURL
	if primary == "" {
		primary = bootstrap.CoderURL
	}

	return primary, &coderv1alpha1.SecretKeySelector{Name: tokenSecretName, Key: proxyTokenKey}, true, nil
}

func (r *WorkspaceProxyReconciler) reconcileDeployment(
	ctx context.Context,
	workspaceProxy *coderv1alpha1.WorkspaceProxy,
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
		deployment.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: maps.Clone(labels)},
			Spec: corev1.PodSpec{
				ImagePullSecrets: workspaceProxy.Spec.ImagePullSecrets,
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
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile workspace proxy deployment: %w", err)
	}

	if err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, deployment); err != nil {
		return nil, fmt.Errorf("get reconciled deployment: %w", err)
	}

	return deployment, nil
}

func (r *WorkspaceProxyReconciler) reconcileService(ctx context.Context, workspaceProxy *coderv1alpha1.WorkspaceProxy) (*corev1.Service, error) {
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

	if err := r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: service.Namespace}, service); err != nil {
		return nil, fmt.Errorf("get reconciled service: %w", err)
	}

	return service, nil
}

func (r *WorkspaceProxyReconciler) reconcileStatus(
	ctx context.Context,
	workspaceProxy *coderv1alpha1.WorkspaceProxy,
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
		Phase: phase,
	}
	if equality.Semantic.DeepEqual(workspaceProxy.Status, nextStatus) {
		return nil
	}

	workspaceProxy.Status = nextStatus
	if err := r.Status().Update(ctx, workspaceProxy); err != nil {
		return fmt.Errorf("update workspace proxy status: %w", err)
	}

	return nil
}

func (r *WorkspaceProxyReconciler) ensureTokenSecret(
	ctx context.Context,
	workspaceProxy *coderv1alpha1.WorkspaceProxy,
	name string,
	key string,
	token string,
) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workspaceProxy.Namespace}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		labels := workspaceProxyLabels(workspaceProxy.Name)
		secret.Labels = maps.Clone(labels)
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data[key] = []byte(token)
		if err := controllerutil.SetControllerReference(workspaceProxy, secret, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("reconcile proxy token secret %q: %w", name, err)
	}

	return nil
}

func (r *WorkspaceProxyReconciler) readSecretValue(ctx context.Context, namespace, name, key string) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
		return "", err
	}

	value, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %q does not contain key %q", name, key)
	}
	if len(value) == 0 {
		return "", fmt.Errorf("secret %q key %q is empty", name, key)
	}

	return string(value), nil
}

// SetupWithManager wires the reconciler into controller-runtime.
func (r *WorkspaceProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil {
		return fmt.Errorf("assertion failed: manager must not be nil")
	}
	if r.Client == nil {
		return fmt.Errorf("assertion failed: reconciler client must not be nil")
	}
	if r.Scheme == nil {
		return fmt.Errorf("assertion failed: reconciler scheme must not be nil")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&coderv1alpha1.WorkspaceProxy{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Named("workspaceproxy").
		Complete(r)
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
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/managed-by": "coder-k8s",
	}
}
