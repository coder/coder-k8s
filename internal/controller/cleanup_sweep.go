package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	// cleanupSettleWindow is how long after the last create a sweep waits,
	// so a create still in flight commits first (#353 invariant 6).
	cleanupSettleWindow = 5 * time.Minute
	// cleanupObjectRecheck is how often a record of a living object is
	// checked again. The delete of the object also wakes the record.
	cleanupObjectRecheck = 30 * time.Minute
	// cleanupUnmatchedRecheck is how often a record that no control plane
	// of its deployment reaches is checked again.
	cleanupUnmatchedRecheck = 10 * time.Minute
	// cleanupRetry is the retry after a failed Coder call.
	cleanupRetry = time.Minute
)

var (
	cleanupRecordsSwept = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "coder_k8s_cleanup_records_swept_total",
		Help: "Cleanup records whose resources a sweep deleted in Coder, or found gone, after their object went without a delete.",
	}, []string{"kind"})
	cleanupRecordsUnmatched = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "coder_k8s_cleanup_records_unmatched",
		Help: "Cleanup records of gone objects that no ready control plane of their Coder deployment reaches.",
	}, []string{"kind"})
)

func init() {
	metrics.Registry.MustRegister(cleanupRecordsSwept, cleanupRecordsUnmatched)
}

// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// CleanupRecordReconciler sweeps the cleanup records of objects that went
// without a confirmed delete in Coder (#353). It acts only through a ready
// control plane whose current Coder deployment ID equals the record's, and
// it deletes only the recorded provider ID or the provider with the
// recorded nonce.
type CleanupRecordReconciler struct {
	client.Client
	// APIReader reads the object of a record live, so a stale cache never
	// makes a living object look gone.
	APIReader         client.Reader
	OperatorNamespace string
	Clock             clock.Clock
	Recorder          events.EventRecorder

	unmatched sync.Map // types.UID -> kind of a record that no deployment reaches.
}

// SetupWithManager watches the records in the operator namespace. It wakes a
// record when its CoderAIProvider is deleted, and every record when a control
// plane gets operator access, since that control plane can match them.
func (r *CleanupRecordReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil || r.Client == nil || r.APIReader == nil || r.OperatorNamespace == "" || r.Clock == nil || r.Recorder == nil {
		return errors.New("assertion failed: the cleanup record reconciler needs a manager, a client, an API reader, the operator namespace, a clock and an event recorder")
	}
	isRecord := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.OperatorNamespace && obj.GetLabels()[cleanupRecordLabel] != ""
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(isRecord)).
		Watches(&coderv1alpha1.CoderAIProvider{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.OperatorNamespace, Name: cleanupRecordObjectName(obj.GetUID())}}}
		}), builder.WithPredicates(dependentRemoved)).
		Watches(&coderv1alpha1.CoderControlPlane{}, handler.EnqueueRequestsFromMapFunc(r.allRecords), builder.WithPredicates(operatorAccessGained)).
		Named("cleanuprecord").
		Complete(r)
}

// operatorAccessGained passes a control plane that has operator access, or
// whose access or URL changed.
var operatorAccessGained = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		cp, ok := e.Object.(*coderv1alpha1.CoderControlPlane)
		return ok && cp.Status.OperatorAccessReady
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		before, ok1 := e.ObjectOld.(*coderv1alpha1.CoderControlPlane)
		after, ok2 := e.ObjectNew.(*coderv1alpha1.CoderControlPlane)
		return ok1 && ok2 && after.Status.OperatorAccessReady &&
			(!before.Status.OperatorAccessReady || before.Status.URL != after.Status.URL)
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// allRecords returns every record in the operator namespace.
func (r *CleanupRecordReconciler) allRecords(ctx context.Context, _ client.Object) []reconcile.Request {
	records := &corev1.ConfigMapList{}
	if err := r.List(ctx, records, client.InNamespace(r.OperatorNamespace), client.HasLabels{cleanupRecordLabel}); err != nil {
		log.FromContext(ctx).Error(err, "Cannot list the cleanup records")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(records.Items))
	for _, record := range records.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: record.Namespace, Name: record.Name}})
	}
	return requests
}

// Reconcile sweeps one record.
func (r *CleanupRecordReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != r.OperatorNamespace {
		return ctrl.Result{}, nil
	}
	configMap := &corev1.ConfigMap{}
	if err := r.Get(ctx, req.NamespacedName, configMap); apierrors.IsNotFound(err) {
		if uid, ok := strings.CutPrefix(req.Name, cleanupRecordPrefix); ok {
			r.setUnmatched(types.UID(uid), "", false)
		}
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}
	record, err := parseCleanupRecord(configMap)
	if err != nil {
		// Never act on a record that the operator did not write.
		log.FromContext(ctx).Error(err, "Ignoring a ConfigMap that is not a valid cleanup record")
		return ctrl.Result{}, nil
	}
	if record.Kind != cleanupRecordKindAIProvider {
		return ctrl.Result{}, nil // Only provider records are swept so far.
	}

	// Act only when the object with that UID no longer exists (invariant 6).
	provider := &coderv1alpha1.CoderAIProvider{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: record.Namespace, Name: record.Name}, provider); err == nil && provider.UID == record.UID {
		r.setUnmatched(record.UID, record.Kind, false)
		return ctrl.Result{RequeueAfter: cleanupObjectRecheck}, nil
	} else if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	settleFrom := record.CreateAttemptTime
	if settleFrom.IsZero() {
		settleFrom = configMap.CreationTimestamp.Time
	}
	if wait := settleFrom.Add(cleanupSettleWindow).Sub(r.Clock.Now()); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	sdk, controlPlane, unknown, err := r.matchingControlPlane(ctx, record)
	if err != nil {
		return ctrl.Result{}, err
	}
	if sdk == nil && unknown {
		// A ready control plane did not answer its deployment ID, for
		// example while its Coder pods start. It can still match.
		return ctrl.Result{RequeueAfter: cleanupRetry}, nil
	}
	if sdk == nil {
		r.setUnmatched(record.UID, record.Kind, true)
		r.Recorder.Eventf(configMap, nil, corev1.EventTypeWarning, "NoMatchingDeployment", "Sweep",
			"No ready CoderControlPlane in namespace %s reaches Coder deployment %s, which holds the provider of deleted %s %s/%s. Delete the provider in Coder by hand, then delete this ConfigMap.",
			record.Namespace, record.DeploymentID, record.Kind, record.Namespace, record.Name)
		return ctrl.Result{RequeueAfter: cleanupUnmatchedRecheck}, nil
	}
	r.setUnmatched(record.UID, record.Kind, false)
	logger := log.FromContext(ctx).WithValues("record", configMap.Name, "deploymentID", record.DeploymentID,
		"controlPlane", types.NamespacedName{Namespace: controlPlane.Namespace, Name: controlPlane.Name}, "provider", record.Namespace+"/"+record.Name)
	gone, why, err := sweepAIProvider(ctx, sdk, record)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !gone {
		logger.Info("The cleanup of a deleted provider in Coder failed, retrying", "why", why)
		return ctrl.Result{RequeueAfter: cleanupRetry}, nil
	}
	// Coder confirmed the delete in the matching deployment (invariant 7).
	if err := deleteCleanupRecord(ctx, r.Client, r.APIReader, r.OperatorNamespace, record.UID); err != nil {
		return ctrl.Result{}, err
	}
	cleanupRecordsSwept.WithLabelValues(record.Kind).Inc()
	logger.Info("Swept the provider of a deleted CoderAIProvider in Coder", "result", why, "providerID", record.CoderID)
	return ctrl.Result{}, nil
}

// matchingControlPlane returns a Coder client of a ready control plane in
// the record's namespace whose current deployment ID is the record's, or nil
// when there is none (#353 invariant 4). The ID comes from Coder now, never
// from the record or a URL. unknown reports a ready control plane whose
// deployment ID read failed.
//
// Only the record's own namespace counts: Coder reports its deployment ID
// itself, so a control plane that someone else runs could claim any ID. The
// records of a namespace, and the control planes that sweep them, are then
// under the control of the same people, and a sweep can only delete what
// their own operator access could delete.
func (r *CleanupRecordReconciler) matchingControlPlane(ctx context.Context, record *cleanupRecord) (*codersdk.Client, *coderv1alpha1.CoderControlPlane, bool, error) {
	deploymentID := record.DeploymentID
	controlPlanes := &coderv1alpha1.CoderControlPlaneList{}
	if err := r.List(ctx, controlPlanes, client.InNamespace(record.Namespace)); err != nil {
		return nil, nil, false, fmt.Errorf("list control planes: %w", err)
	}
	unknown := false
	for i := range controlPlanes.Items {
		cp := &controlPlanes.Items[i]
		// A live read: the cache can show the control plane ready before
		// it shows its new operator token Secret.
		sdk, _, wait, err := controlPlaneCoderClient(ctx, r.APIReader, cp.Namespace, cp.Name)
		if err != nil || wait != nil {
			// A control plane that reports operator access can still match
			// once its access works.
			unknown = unknown || cp.Status.OperatorAccessReady
			continue
		}
		id, err := coderDeploymentID(ctx, sdk)
		if err != nil {
			log.FromContext(ctx).V(1).Info("Cannot read the deployment ID of a control plane", "controlPlane", cp.Namespace+"/"+cp.Name, "error", coderErrorSummary(err))
			unknown = true
			continue
		}
		if id == deploymentID {
			return sdk, cp, false, nil
		}
	}
	return nil, nil, unknown, nil
}

// sweepAIProvider deletes the provider of record in Coder. It returns gone
// when Coder confirms that it holds no provider of the record: a delete
// answered with success or Coder's own 404, a read with Coder's own 404 for
// a site owner, or a provider of that name without the record's nonce. It
// deletes only the recorded ID or the provider whose display name is the
// recorded nonce (invariant 5).
func sweepAIProvider(ctx context.Context, sdk *codersdk.Client, record *cleanupRecord) (gone bool, why string, err error) {
	if record.Nonce != aiProviderNoncePrefix+string(record.UID) {
		return false, "", fmt.Errorf("assertion failed: cleanup record of %s has nonce %q, not the nonce of its UID", record.UID, record.Nonce)
	}
	if step := requireSiteOwner(ctx, sdk); step != nil {
		// Only a site owner can trust Coder's 404 for a provider.
		return false, step.message, nil
	}
	id := record.CoderID
	if id != "" {
		if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil || parsed.String() != id {
			return false, "", fmt.Errorf("assertion failed: cleanup record of %s has coderID %q, which is not a canonical UUID", record.UID, id)
		}
	} else {
		found, err := sdk.AIProvider(ctx, record.Name)
		switch {
		case isCoderResourceNotFound(err):
			return true, "no provider of that name", nil
		case err != nil:
			return false, "get provider: " + coderErrorSummary(err), nil
		}
		if err := coderAnswerFor("AI provider", record.Name, found.Name); err != nil {
			return false, err.Error(), nil //nolint:nilerr // A wrong answer retries the sweep.
		}
		if found.DisplayName != record.Nonce {
			// Another provider holds the name, so no create of this object
			// committed after the settle window.
			return true, "the provider of that name is not this object's", nil
		}
		if found.ID == uuid.Nil {
			return false, "", errors.New("assertion failed: Coder answered an AI provider without an ID")
		}
		id = found.ID.String()
	}
	if err := sdk.DeleteAIProvider(ctx, id); err != nil && !isCoderResourceNotFound(err) {
		return false, "delete provider " + id + ": " + coderErrorSummary(err), nil
	} else if err != nil {
		return true, "provider " + id + " was already gone", nil //nolint:nilerr // Coder's own 404 confirms the delete.
	}
	return true, "deleted provider " + id, nil
}

// setUnmatched keeps the unmatched gauge equal to the records that no
// deployment reaches.
func (r *CleanupRecordReconciler) setUnmatched(uid types.UID, kind string, unmatched bool) {
	if unmatched {
		r.unmatched.Store(uid, kind)
	} else {
		r.unmatched.Delete(uid)
	}
	counts := map[string]float64{cleanupRecordKindAIProvider: 0}
	r.unmatched.Range(func(_, v any) bool {
		counts[v.(string)]++
		return true
	})
	for k, n := range counts {
		cleanupRecordsUnmatched.WithLabelValues(k).Set(n)
	}
}
