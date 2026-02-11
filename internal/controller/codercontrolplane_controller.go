// Package controller contains Kubernetes controllers for coder-k8s resources.
package controller

import (
	"context"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	defaultCoderImage       = "ghcr.io/coder/coder:latest"
	defaultControlPlanePort = int32(80)
	controlPlaneTargetPort  = int32(3000)
)

// CoderControlPlaneReconciler reconciles a CoderControlPlane object.
type CoderControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

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

	if err := r.reconcileStatus(ctx, coderControlPlane, deployment, service); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
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

func (r *CoderControlPlaneReconciler) reconcileStatus(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	deployment *appsv1.Deployment,
	service *corev1.Service,
) error {
	servicePort := coderControlPlane.Spec.Service.Port
	if servicePort == 0 {
		servicePort = defaultControlPlanePort
	}

	phase := coderv1alpha1.CoderControlPlanePhasePending
	if deployment.Status.ReadyReplicas > 0 {
		phase = coderv1alpha1.CoderControlPlanePhaseReady
	}

	nextStatus := coderv1alpha1.CoderControlPlaneStatus{
		ObservedGeneration: coderControlPlane.Generation,
		ReadyReplicas:      deployment.Status.ReadyReplicas,
		URL:                fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", service.Name, service.Namespace, servicePort),
		Phase:              phase,
	}
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&coderv1alpha1.CoderControlPlane{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
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
