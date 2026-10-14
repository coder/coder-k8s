package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// controlPlaneDependentHold is how long a CoderControlPlane that is being
// deleted waits for the CoderAIGatewayKey and CoderAIProvider objects that
// use it (#353). Its Coder Deployment keeps running while it waits, so they
// can delete their keys and providers in Coder. It covers the 5-minute
// settle window of a key deletion plus retries. After it, the control plane
// goes, and they release with ControlPlaneGone.
const controlPlaneDependentHold = 10 * time.Minute

// controlPlaneDependentGrace is how long a control plane stays after its
// hold ends while dependents remain, so each of them sees the hold end and
// runs its bounded pass while Coder still runs. Dependents requeue before the
// hold ends (controlPlaneHoldRequeue), so they need only a short time.
const controlPlaneDependentGrace = 30 * time.Second

// controlPlaneDependentPoll is the requeue of a control plane that waits for
// its dependents. A dependent's removal also requeues it at once.
const controlPlaneDependentPoll = 10 * time.Second

// releasedWithoutDelete counts finalizers that went while their keys or
// providers can stay in Coder (#353), by kind. Today the release log line is
// the only other signal. countReleasedWithoutDelete records a release, and
// releasedWithoutDeleteDone counts it once the finalizer is gone, so a
// failed finalizer patch and its retry count once.
var releasedWithoutDelete = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "coder_k8s_cleanup_released_without_delete_total",
	Help: "Finalizers released without deleting the object's resources in Coder, because Coder was not reachable.",
}, []string{"kind"})

var pendingReleases sync.Map // types.UID -> kind label

func init() {
	metrics.Registry.MustRegister(releasedWithoutDelete)
}

func countReleasedWithoutDelete(uid types.UID, kind string) { pendingReleases.Store(uid, kind) }

// releasedWithoutDeleteDone counts the release of uid, if one was recorded,
// after its finalizer patch succeeded.
func releasedWithoutDeleteDone(uid types.UID) {
	if kind, ok := pendingReleases.LoadAndDelete(uid); ok {
		releasedWithoutDelete.WithLabelValues(kind.(string)).Inc()
	}
}

// controlPlaneHoldEnd is when the hold of a control plane that is being
// deleted ends.
func controlPlaneHoldEnd(controlPlane *coderv1alpha1.CoderControlPlane) time.Time {
	return controlPlane.DeletionTimestamp.Add(controlPlaneDependentHold)
}

// controlPlaneHoldRequeue caps a dependent's requeue under the hold, so it
// looks again right after the hold ends, inside the grace period.
func controlPlaneHoldRequeue(controlPlane *coderv1alpha1.CoderControlPlane, now time.Time, requeue time.Duration) time.Duration {
	return min(requeue, max(controlPlaneHoldEnd(controlPlane).Sub(now)+time.Second, time.Second))
}

// controlPlaneHolds reports whether controlPlane, which is being deleted,
// waits for its dependents at now: it still has its finalizer, the hold time
// has not passed, it is not deleted in the foreground, and its namespace is
// not being deleted. A namespace deletion removes Coder's pods at once, and a
// foreground deletion removes the Coder Deployment (blockOwnerDeletion)
// before the control plane, so no hold can keep Coder reachable then.
func controlPlaneHolds(ctx context.Context, reader client.Reader, controlPlane *coderv1alpha1.CoderControlPlane, now time.Time) (bool, error) {
	if reader == nil || controlPlane == nil {
		return false, errors.New("assertion failed: the control plane hold check needs a reader and a control plane")
	}
	if controlPlane.DeletionTimestamp.IsZero() {
		return false, errors.New("assertion failed: the control plane hold applies only while the control plane is being deleted")
	}
	if !controllerutil.ContainsFinalizer(controlPlane, workspaceRBACFinalizer) || controllerutil.ContainsFinalizer(controlPlane, metav1.FinalizerDeleteDependents) ||
		!now.Before(controlPlaneHoldEnd(controlPlane)) {
		return false, nil
	}
	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, types.NamespacedName{Name: controlPlane.Namespace}, namespace); err != nil {
		return false, fmt.Errorf("get namespace %s: %w", controlPlane.Namespace, err)
	}
	return namespace.DeletionTimestamp.IsZero() && namespace.Status.Phase != corev1.NamespaceTerminating, nil
}

// controlPlaneDependents returns the CoderAIGatewayKey and CoderAIProvider
// objects in controlPlane's namespace that reference it, as kind/name.
func controlPlaneDependents(ctx context.Context, reader client.Reader, controlPlane *coderv1alpha1.CoderControlPlane) ([]string, error) {
	var dependents []string
	keys := &coderv1alpha1.CoderAIGatewayKeyList{}
	if err := reader.List(ctx, keys, client.InNamespace(controlPlane.Namespace)); err != nil {
		return nil, fmt.Errorf("list AI Gateway keys of namespace %s: %w", controlPlane.Namespace, err)
	}
	for _, k := range keys.Items {
		if k.Spec.ControlPlaneRef.Name == controlPlane.Name {
			dependents = append(dependents, "CoderAIGatewayKey/"+k.Name)
		}
	}
	providers := &coderv1alpha1.CoderAIProviderList{}
	if err := reader.List(ctx, providers, client.InNamespace(controlPlane.Namespace)); err != nil {
		return nil, fmt.Errorf("list AI providers of namespace %s: %w", controlPlane.Namespace, err)
	}
	for _, p := range providers.Items {
		if p.Spec.ControlPlaneRef.Name == controlPlane.Name {
			dependents = append(dependents, "CoderAIProvider/"+p.Name)
		}
	}
	return dependents, nil
}

// dependentRemoved passes only the removal of a dependent object, the event
// that can end a control plane's hold.
var dependentRemoved = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return false },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// requestForDependentControlPlane maps a CoderAIGatewayKey or CoderAIProvider
// to the control plane that it references.
func requestForDependentControlPlane(_ context.Context, obj client.Object) []reconcile.Request {
	var name string
	switch o := obj.(type) {
	case *coderv1alpha1.CoderAIGatewayKey:
		name = o.Spec.ControlPlaneRef.Name
	case *coderv1alpha1.CoderAIProvider:
		name = o.Spec.ControlPlaneRef.Name
	}
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}
