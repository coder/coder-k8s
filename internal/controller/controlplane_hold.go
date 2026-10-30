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
// deleted waits for the CoderAIGatewayKey, CoderAIProvider and CoderMCPServer
// objects that use it (#353, #382). Its Coder Deployment keeps running while
// it waits, so they can delete their keys, providers and MCP server configs
// in Coder. It covers the 5-minute
// settle window of a key deletion plus retries. After it, the control plane
// goes, and they release with ControlPlaneGone.
const controlPlaneDependentHold = 10 * time.Minute

// controlPlaneDependentGrace is the base of how long a control plane stays
// after its hold ends while dependents remain, so each of them sees the hold
// end and runs its bounded pass while Coder still runs. Dependents requeue
// before the hold ends (controlPlaneHoldRequeue). One worker runs the passes
// of one kind one after the other, so controlPlaneDependentGraceFor adds one
// bounded pass per dependent of the larger kind.
const controlPlaneDependentGrace = 30 * time.Second

// controlPlaneDependentGraceMax caps the grace period, so dependents that
// someone keeps creating cannot keep the control plane forever.
const controlPlaneDependentGraceMax = 5 * time.Minute

// controlPlaneLiveDependentWait is how long a deleted control plane waits for
// a dependent that is not being deleted itself. It covers one command that
// deletes the control plane before its dependents (kubectl delete -f). After
// it, only dependents that are being deleted hold the control plane: a live
// dependent does no cleanup, so a longer wait for it does nothing.
const controlPlaneLiveDependentWait = 30 * time.Second

// controlPlaneDependentPoll is the requeue of a control plane that waits for
// its dependents. A dependent's removal also requeues it at once.
const controlPlaneDependentPoll = 10 * time.Second

// releasedWithoutDelete counts finalizers that went while their keys,
// providers or MCP server configs can stay in Coder (#353), by kind. Today the release log line is
// the only other signal. countReleasedWithoutDelete records a release, and
// releasedWithoutDeleteDone counts it once the finalizer is gone, so a
// failed finalizer patch and its retry count once. Each delete reconcile
// first drops an earlier record (forgetReleasedWithoutDelete), so a retry
// that then deletes in Coder does not count.
var releasedWithoutDelete = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "coder_k8s_cleanup_released_without_delete_total",
	Help: "Finalizers released without deleting the object's resources in Coder, because Coder was not reachable.",
}, []string{"kind"})

var pendingReleases sync.Map // types.UID -> kind label

func init() {
	metrics.Registry.MustRegister(releasedWithoutDelete)
}

func countReleasedWithoutDelete(uid types.UID, kind string) { pendingReleases.Store(uid, kind) }

// forgetReleasedWithoutDelete drops a release of uid that an earlier
// reconcile recorded but whose finalizer patch failed.
func forgetReleasedWithoutDelete(uid types.UID) { pendingReleases.Delete(uid) }

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

// controlPlaneDependentGraceFor is the grace period after the hold when
// perKind dependents of one kind remain (controlPlaneDependentGrace).
func controlPlaneDependentGraceFor(perKind int) time.Duration {
	return min(controlPlaneDependentGrace+time.Duration(perKind)*max(aiGatewayKeyBoundedDeleteTimeout, aiProviderBoundedDeleteTimeout, mcpServerBoundedDeleteTimeout), controlPlaneDependentGraceMax)
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

// controlPlaneDependents returns the CoderAIGatewayKey, CoderAIProvider and
// CoderMCPServer objects in controlPlane's namespace that reference it and hold it at now,
// as kind/name, and the larger count of one kind. A dependent holds it while
// it is being deleted and still has its cleanup finalizer (another finalizer
// does not hold it), and a live one only for controlPlaneLiveDependentWait
// after the control plane deletion.
func controlPlaneDependents(ctx context.Context, reader client.Reader, controlPlane *coderv1alpha1.CoderControlPlane, now time.Time) ([]string, int, error) {
	if controlPlane.DeletionTimestamp.IsZero() {
		return nil, 0, errors.New("assertion failed: dependents hold only a control plane that is being deleted")
	}
	liveHolds := now.Before(controlPlane.DeletionTimestamp.Add(controlPlaneLiveDependentWait))
	holds := func(o client.Object, finalizer string) bool {
		if o.GetDeletionTimestamp().IsZero() {
			return liveHolds
		}
		return controllerutil.ContainsFinalizer(o, finalizer)
	}
	var dependents []string
	keys := &coderv1alpha1.CoderAIGatewayKeyList{}
	if err := reader.List(ctx, keys, client.InNamespace(controlPlane.Namespace)); err != nil {
		return nil, 0, fmt.Errorf("list AI Gateway keys of namespace %s: %w", controlPlane.Namespace, err)
	}
	for i := range keys.Items {
		if k := &keys.Items[i]; k.Spec.ControlPlaneRef.Name == controlPlane.Name && holds(k, coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer) {
			dependents = append(dependents, "CoderAIGatewayKey/"+k.Name)
		}
	}
	keyCount := len(dependents)
	providers := &coderv1alpha1.CoderAIProviderList{}
	if err := reader.List(ctx, providers, client.InNamespace(controlPlane.Namespace)); err != nil {
		return nil, 0, fmt.Errorf("list AI providers of namespace %s: %w", controlPlane.Namespace, err)
	}
	for i := range providers.Items {
		if p := &providers.Items[i]; p.Spec.ControlPlaneRef.Name == controlPlane.Name && holds(p, coderv1alpha1.CoderAIProviderCleanupFinalizer) {
			dependents = append(dependents, "CoderAIProvider/"+p.Name)
		}
	}
	providerCount := len(dependents) - keyCount
	servers := &coderv1alpha1.CoderMCPServerList{}
	if err := reader.List(ctx, servers, client.InNamespace(controlPlane.Namespace)); err != nil {
		return nil, 0, fmt.Errorf("list MCP servers of namespace %s: %w", controlPlane.Namespace, err)
	}
	for i := range servers.Items {
		if m := &servers.Items[i]; m.Spec.ControlPlaneRef.Name == controlPlane.Name && holds(m, coderv1alpha1.CoderMCPServerCleanupFinalizer) {
			dependents = append(dependents, "CoderMCPServer/"+m.Name)
		}
	}
	return dependents, max(keyCount, providerCount, len(dependents)-keyCount-providerCount), nil
}

// dependentRemoved passes only the removal of a dependent object, the event
// that can end a control plane's hold.
var dependentRemoved = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return false },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// requestForDependentControlPlane maps a CoderAIGatewayKey, CoderAIProvider or
// CoderMCPServer to the control plane that it references.
func requestForDependentControlPlane(_ context.Context, obj client.Object) []reconcile.Request {
	var name string
	switch o := obj.(type) {
	case *coderv1alpha1.CoderAIGatewayKey:
		name = o.Spec.ControlPlaneRef.Name
	case *coderv1alpha1.CoderAIProvider:
		name = o.Spec.ControlPlaneRef.Name
	case *coderv1alpha1.CoderMCPServer:
		name = o.Spec.ControlPlaneRef.Name
	}
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}
