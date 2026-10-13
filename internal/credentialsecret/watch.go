package credentialsecret

import (
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// WatchPredicate passes the Secret events that can change a Read result:
// creates and updates where the old or the new Secret carries the opt-in
// label, and every delete. Events for unlabeled Secrets cannot turn a failed
// Read into a pass, so they are dropped.
func WatchPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return hasLabel(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return hasLabel(e.ObjectOld) || hasLabel(e.ObjectNew)
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return true
		},
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
	}
}

func hasLabel(obj client.Object) bool {
	if obj == nil {
		return false
	}
	if _, ok := obj.(*corev1.Secret); !ok {
		return false
	}
	_, ok := obj.GetLabels()[LabelKey]
	return ok
}
