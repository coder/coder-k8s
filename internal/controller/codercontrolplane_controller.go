package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// CoderControlPlaneReconciler reconciles a CoderControlPlane object.
type CoderControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coder.com,resources=codercontrolplanes/finalizers,verbs=update

// Reconcile implements the baseline no-op reconciliation loop.
func (r *CoderControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: reconciler client must not be nil")
	}
	if r.Scheme == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: reconciler scheme must not be nil")
	}

	logger := log.FromContext(ctx).WithValues("coderControlPlane", req.NamespacedName)
	coderControlPlane := &coderv1alpha1.CoderControlPlane{}

	if err := r.Get(ctx, req.NamespacedName, coderControlPlane); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("resource deleted before reconciliation")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get codercontrolplane %s: %w", req.NamespacedName, err)
	}

	if coderControlPlane.Name != req.Name || coderControlPlane.Namespace != req.Namespace {
		return ctrl.Result{}, fmt.Errorf("assertion failed: fetched object %s/%s does not match request %s/%s",
			coderControlPlane.Namespace, coderControlPlane.Name, req.Namespace, req.Name)
	}

	// TODO: add finalizer handling.
	// TODO: reconcile dependent resources.
	// TODO: update status conditions and phases.
	logger.V(1).Info("reconciled placeholder resource")
	return ctrl.Result{}, nil
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
		Named("codercontrolplane").
		Complete(r)
}
