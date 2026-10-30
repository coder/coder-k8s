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
// it deletes only the recorded provider or MCP server config ID, the provider
// or config with the recorded nonce, or the AI Gateway keys whose names
// start with the recorded prefix.
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
// record when its CoderAIProvider, CoderAIGatewayKey or CoderMCPServer is
// deleted, and every record when a control plane gets operator access, since
// that control plane can match them.
func (r *CleanupRecordReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil || r.Client == nil || r.APIReader == nil || r.OperatorNamespace == "" || r.Clock == nil || r.Recorder == nil {
		return errors.New("assertion failed: the cleanup record reconciler needs a manager, a client, an API reader, the operator namespace, a clock and an event recorder")
	}
	isRecord := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.OperatorNamespace && obj.GetLabels()[cleanupRecordLabel] != ""
	})
	recordOf := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.OperatorNamespace, Name: cleanupRecordObjectName(obj.GetUID())}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(isRecord)).
		Watches(&coderv1alpha1.CoderAIProvider{}, recordOf, builder.WithPredicates(dependentRemoved)).
		Watches(&coderv1alpha1.CoderAIGatewayKey{}, recordOf, builder.WithPredicates(dependentRemoved)).
		Watches(&coderv1alpha1.CoderMCPServer{}, recordOf, builder.WithPredicates(dependentRemoved)).
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
	// A live read: the cache can still hold an older create time.
	configMap := &corev1.ConfigMap{}
	if err := r.APIReader.Get(ctx, req.NamespacedName, configMap); apierrors.IsNotFound(err) {
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
	kind, ok := cleanupRecordKinds[record.Kind]
	if !ok {
		return ctrl.Result{}, nil // A kind that this operator does not sweep.
	}

	// Act only when the object with that UID no longer exists (invariant 6).
	object := kind.object()
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: record.Namespace, Name: record.Name}, object); err == nil && object.GetUID() == record.UID {
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

	matches, unknown, err := r.matchingControlPlanes(ctx, record)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(matches) == 0 && unknown {
		// A ready control plane did not answer its deployment ID, for
		// example while its Coder pods start. It can still match.
		return ctrl.Result{RequeueAfter: cleanupRetry}, nil
	}
	if len(matches) == 0 {
		r.setUnmatched(record.UID, record.Kind, true)
		r.Recorder.Eventf(configMap, nil, corev1.EventTypeWarning, "NoMatchingDeployment", "Sweep",
			"No ready CoderControlPlane in namespace %s reaches Coder deployment %s, which holds %s of deleted %s %s/%s. Delete %s in Coder by hand, then delete this ConfigMap.",
			record.Namespace, record.DeploymentID, kind.what, record.Kind, record.Namespace, record.Name, kind.what)
		return ctrl.Result{RequeueAfter: cleanupUnmatchedRecheck}, nil
	}
	r.setUnmatched(record.UID, record.Kind, false)
	logger := log.FromContext(ctx).WithValues("record", configMap.Name, "deploymentID", record.DeploymentID, "object", record.Kind+" "+record.Namespace+"/"+record.Name)
	// Each matching control plane has its own operator token, so a failure
	// through one tries the next.
	gone, why, through := false, "", logger
	for _, m := range matches {
		through = logger.WithValues("controlPlane", types.NamespacedName{Namespace: m.controlPlane.Namespace, Name: m.controlPlane.Name})
		if gone, why, err = kind.sweep(ctx, m.sdk, record); err != nil {
			return ctrl.Result{}, err
		} else if gone {
			break
		}
		through.Info("The cleanup of a deleted object in Coder failed", "why", why)
	}
	if !gone {
		return ctrl.Result{RequeueAfter: cleanupRetry}, nil
	}
	// Coder confirmed the delete in the matching deployment (invariant 7).
	if err := deleteCleanupRecord(ctx, r.Client, r.APIReader, r.OperatorNamespace, record.UID); err != nil {
		return ctrl.Result{}, err
	}
	cleanupRecordsSwept.WithLabelValues(record.Kind).Inc()
	through.Info("Swept the Coder resources of a deleted object", "result", why, "coderID", record.CoderID)
	return ctrl.Result{}, nil
}

// cleanupSweepKind is how a sweep handles the records of one object kind.
type cleanupSweepKind struct {
	object func() client.Object
	what   string // What the object holds in Coder, for the Event.
	sweep  func(context.Context, *codersdk.Client, *cleanupRecord) (gone bool, why string, err error)
}

var cleanupRecordKinds = map[string]cleanupSweepKind{
	cleanupRecordKindAIProvider: {
		object: func() client.Object { return &coderv1alpha1.CoderAIProvider{} }, what: "the provider", sweep: sweepAIProvider,
	},
	cleanupRecordKindAIGatewayKey: {
		object: func() client.Object { return &coderv1alpha1.CoderAIGatewayKey{} }, what: "the AI Gateway keys", sweep: sweepAIGatewayKeys,
	},
	cleanupRecordKindMCPServer: {
		object: func() client.Object { return &coderv1alpha1.CoderMCPServer{} }, what: "the MCP server config", sweep: sweepMCPServer,
	},
}

// controlPlaneMatch is a control plane whose Coder deployment is a record's.
type controlPlaneMatch struct {
	sdk          *codersdk.Client
	controlPlane *coderv1alpha1.CoderControlPlane
}

// matchingControlPlanes returns the Coder clients of the ready control planes
// in the record's namespace whose current deployment ID is the record's
// (#353 invariant 4). The ID comes from Coder now, never
// from the record or a URL. unknown reports a ready control plane whose
// deployment ID read failed.
//
// Only the record's own namespace counts: Coder reports its deployment ID
// itself, so a control plane that someone else runs could claim any ID. The
// records of a namespace, and the control planes that sweep them, are then
// under the control of the same people, and a sweep can only delete what
// their own operator access could delete.
func (r *CleanupRecordReconciler) matchingControlPlanes(ctx context.Context, record *cleanupRecord) ([]controlPlaneMatch, bool, error) {
	deploymentID := record.DeploymentID
	controlPlanes := &coderv1alpha1.CoderControlPlaneList{}
	if err := r.List(ctx, controlPlanes, client.InNamespace(record.Namespace)); err != nil {
		return nil, false, fmt.Errorf("list control planes: %w", err)
	}
	unknown := false
	var matches []controlPlaneMatch
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
			matches = append(matches, controlPlaneMatch{sdk: sdk, controlPlane: cp})
		}
	}
	return matches, unknown, nil
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
		if found.DisplayName != record.Nonce || string(found.Type) != record.Type {
			// Another provider holds the name, so no create of this object
			// committed after the settle window. The nonce and the type
			// prove ownership, as on the controller's own paths.
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

// sweepAIGatewayKeys deletes the keys of record in Coder: every key whose
// name is the record's prefix ck8s-<uid>- and a serial (invariant 5). The
// object is gone, so no serial bound applies: the settle window has passed,
// and no create of the object can still commit. It returns gone only when a
// LIST, read by a site owner, shows no such key.
func sweepAIGatewayKeys(ctx context.Context, sdk *codersdk.Client, record *cleanupRecord) (gone bool, why string, err error) {
	prefix, err := aiGatewayKeyNamePrefix(record.UID)
	if err != nil {
		return false, "", err
	}
	if record.Nonce != prefix {
		return false, "", fmt.Errorf("assertion failed: cleanup record of %s has key prefix %q, not the prefix of its UID", record.UID, record.Nonce)
	}
	if step := requireSiteOwner(ctx, sdk); step != nil {
		// Only a site owner's LIST shows every key.
		return false, step.message, nil
	}
	deleted := 0
	for range 2 {
		listed, err := sdk.ListAIGatewayKeys(ctx)
		if err != nil {
			return false, "list AI Gateway keys: " + coderErrorSummary(err), nil
		}
		own := ownAIGatewayKeys(prefix, listed)
		if len(own) == 0 {
			return true, fmt.Sprintf("deleted %d keys, and a list shows none left", deleted), nil
		}
		for _, k := range own {
			// Only Coder's own 404 proves that the key is gone (#325).
			if err := sdk.DeleteAIGatewayKey(ctx, k.ID); err != nil && !isCoderResourceNotFound(err) {
				return false, "delete key " + k.Name + ": " + coderErrorSummary(err), nil
			}
			deleted++
		}
	}
	return false, "Coder still lists keys of the object after their delete", nil
}

// sweepMCPServer deletes the MCP server config of record in Coder. It
// returns gone when Coder confirms that it holds no config of the record: a
// delete answered with success or Coder's own 404, or a list by a site owner
// that shows no config of the record's slug with the record's nonce. It
// deletes only the recorded ID or the config whose display name is the
// recorded nonce (invariant 5).
func sweepMCPServer(ctx context.Context, sdk *codersdk.Client, record *cleanupRecord) (gone bool, why string, err error) {
	if record.Nonce != mcpServerNoncePrefix+string(record.UID) {
		return false, "", fmt.Errorf("assertion failed: cleanup record of %s has nonce %q, not the nonce of its UID", record.UID, record.Nonce)
	}
	orgID, err := uuid.Parse(record.Organization)
	if err != nil || orgID == uuid.Nil || orgID.String() != record.Organization || record.Slug == "" {
		return false, "", fmt.Errorf("assertion failed: cleanup record of %s has organization ID %q and slug %q", record.UID, record.Organization, record.Slug)
	}
	if step := mcpServerRequireSiteOwner(ctx, sdk); step != nil {
		// Only a site owner sees every config in the list.
		return false, step.message, nil
	}
	var id uuid.UUID
	if record.CoderID != "" {
		if id, err = uuid.Parse(record.CoderID); err != nil || id == uuid.Nil || id.String() != record.CoderID {
			return false, "", fmt.Errorf("assertion failed: cleanup record of %s has coderID %q, which is not a canonical UUID", record.UID, record.CoderID)
		}
	} else {
		configs, err := sdk.MCPServerConfigs(ctx, orgID)
		switch {
		case isCoderResourceNotFound(err):
			return true, "the organization is gone", nil
		case err != nil:
			return false, "list MCP server configs: " + coderErrorSummary(err), nil
		}
		found, step := mcpServerWithSlug(configs, record.Slug, orgID)
		switch {
		case step != nil:
			return false, step.message, nil
		case found == nil:
			return true, "no config with that slug", nil
		case found.DisplayName != record.Nonce:
			// Another config holds the slug, so no create of this object
			// committed after the settle window.
			return true, "the config with that slug is not this object's", nil
		}
		id = found.ID
	}
	if err := sdk.DeleteMCPServerConfig(ctx, orgID, id); err != nil && !isCoderResourceNotFound(err) {
		return false, "delete MCP server config " + id.String() + ": " + coderErrorSummary(err), nil
	} else if err != nil {
		return true, "MCP server config " + id.String() + " was already gone", nil //nolint:nilerr // Coder's own 404 confirms the delete.
	}
	return true, "deleted MCP server config " + id.String(), nil
}

// setUnmatched keeps the unmatched gauge equal to the records that no
// deployment reaches.
func (r *CleanupRecordReconciler) setUnmatched(uid types.UID, kind string, unmatched bool) {
	if unmatched {
		r.unmatched.Store(uid, kind)
	} else {
		r.unmatched.Delete(uid)
	}
	counts := map[string]float64{cleanupRecordKindAIProvider: 0, cleanupRecordKindAIGatewayKey: 0, cleanupRecordKindMCPServer: 0}
	r.unmatched.Range(func(_, v any) bool {
		counts[v.(string)]++
		return true
	})
	for k, n := range counts {
		cleanupRecordsUnmatched.WithLabelValues(k).Set(n)
	}
}
