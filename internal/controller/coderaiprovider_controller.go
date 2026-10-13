package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

const (
	aiProviderControlPlaneRefIndex = ".spec.controlPlaneRef.name"
	// aiProviderResync re-reads Coder for objects that wait on Coder state,
	// for example a name conflict that an admin can remove in the dashboard.
	aiProviderResync = 10 * time.Minute
	// aiProviderRetry is the retry for waits that no watch ends: a failed
	// Coder call, or an operator token Secret that is not readable yet.
	aiProviderRetry = 30 * time.Second
)

// CoderAIProviderReconciler reconciles CoderAIProvider objects (#154). It is
// not registered yet. It checks the spec, the credential Secret and the
// control plane, creates the Coder provider when the name is free, and sets
// the spec values on the provider that it created. It never changes a
// provider that it did not create.
type CoderAIProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// aiProviderStep is the outcome of one reconcile. A ready step reports that
// Coder matches the spec. A stalled step waits for a change of the spec or an
// input. Any other step waits for progress.
type aiProviderStep struct {
	ready   bool
	stalled bool
	reason  string
	message string
	requeue time.Duration
}

func aiProviderWait(requeue time.Duration, reason, format string, args ...any) *aiProviderStep {
	return &aiProviderStep{reason: reason, message: fmt.Sprintf(format, args...), requeue: requeue}
}

func aiProviderStalled(requeue time.Duration, reason, format string, args ...any) *aiProviderStep {
	return &aiProviderStep{stalled: true, reason: reason, message: fmt.Sprintf(format, args...), requeue: requeue}
}

// SetupWithManager wires the reconciler into controller-runtime. Nothing
// calls it until activation. A change of the referenced control plane or of
// the key Secret requeues the provider.
func (r *CoderAIProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil || r.Client == nil || r.Scheme == nil {
		return fmt.Errorf("assertion failed: AI provider reconciler needs a manager, a client, and a scheme")
	}
	indexes := map[string]func(*coderv1alpha1.CoderAIProvider) string{
		aiProviderControlPlaneRefIndex:  func(p *coderv1alpha1.CoderAIProvider) string { return p.Spec.ControlPlaneRef.Name },
		credentialSecretNamesFieldIndex: func(p *coderv1alpha1.CoderAIProvider) string { return p.Spec.APIKeySecretRef.Name },
	}
	for name, value := range indexes {
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), &coderv1alpha1.CoderAIProvider{}, name, func(obj client.Object) []string {
			if p, ok := obj.(*coderv1alpha1.CoderAIProvider); ok && value(p) != "" {
				return []string{value(p)}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("index AI providers by %s: %w", name, err)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&coderv1alpha1.CoderAIProvider{}).
		Watches(&coderv1alpha1.CoderControlPlane{}, handler.EnqueueRequestsFromMapFunc(r.providersForControlPlane)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			return r.providersWith(ctx, obj.GetNamespace(), credentialSecretNamesFieldIndex, obj.GetName())
		}),
			builder.WithPredicates(credentialsecret.WatchPredicate())).
		Named("coderaiprovider").
		Complete(r)
}

// providersForControlPlane maps a control plane to the providers that use it,
// and to the providers whose key Secret it names as its operator token
// Secret, because credentialsecret.Read refuses those.
func (r *CoderAIProviderReconciler) providersForControlPlane(ctx context.Context, obj client.Object) []reconcile.Request {
	requests := r.providersWith(ctx, obj.GetNamespace(), aiProviderControlPlaneRefIndex, obj.GetName())
	controlPlane, ok := obj.(*coderv1alpha1.CoderControlPlane)
	if !ok {
		return requests
	}
	tokenSecrets := []string{credentialsecret.OperatorTokenSecretName(controlPlane)}
	if ref := controlPlane.Status.OperatorTokenSecretRef; ref != nil {
		tokenSecrets = append(tokenSecrets, strings.TrimSpace(ref.Name))
	}
	for _, name := range sortedUnique(tokenSecrets) {
		requests = append(requests, r.providersWith(ctx, obj.GetNamespace(), credentialSecretNamesFieldIndex, name)...)
	}
	return requests
}

// providersWith returns requests for the providers in namespace whose index
// holds value.
func (r *CoderAIProviderReconciler) providersWith(ctx context.Context, namespace, index, value string) []reconcile.Request {
	var providers coderv1alpha1.CoderAIProviderList
	if err := r.List(ctx, &providers, client.InNamespace(namespace), client.MatchingFields{index: value}); err != nil {
		log.FromContext(ctx).Error(err, "list AI providers", "index", index, "value", value)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(providers.Items))
	for _, p := range providers.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name}})
	}
	return requests
}

// Reconcile runs the checks in order and reports the first one that fails.
func (r *CoderAIProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil || r.Scheme == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: AI provider reconciler needs a client and a scheme")
	}
	provider := &coderv1alpha1.CoderAIProvider{}
	if err := r.Get(ctx, req.NamespacedName, provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if provider.Name != req.Name || provider.Namespace != req.Namespace {
		return ctrl.Result{}, fmt.Errorf("assertion failed: fetched AI provider %s/%s does not match %s", provider.Namespace, provider.Name, req.NamespacedName)
	}
	before := provider.Status.DeepCopy()

	if !provider.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(provider, coderv1alpha1.CoderAIProviderCleanupFinalizer) {
			return ctrl.Result{}, nil
		}
		if provider.Status.ProviderID == "" && provider.Status.CreateAttemptTime == nil {
			// Nothing of this provider can exist in Coder.
			return ctrl.Result{}, r.patchFinalizer(ctx, provider, false)
		}
		// Keep the finalizer: this controller version cannot prove what Coder
		// holds, so it neither deletes nor forgets the provider.
		applyAIProviderStep(provider, aiProviderWait(0, "DeletePending",
			"Provider %s can exist in Coder. This controller version keeps the finalizer and deletes nothing.", provider.Name))
		return ctrl.Result{}, r.writeStatus(ctx, provider, before)
	}

	if err := r.patchFinalizer(ctx, provider, true); err != nil {
		return ctrl.Result{}, err
	}
	step, err := r.check(ctx, provider, before)
	if err != nil {
		return ctrl.Result{}, err
	}
	applyAIProviderStep(provider, step)
	if err := r.writeStatus(ctx, provider, before); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: step.requeue}, nil
}

// check validates the base URL, reads the key through the credential Secret
// contract, gets Coder access, and then creates or applies the provider. The
// checks before Coder access send nothing to Coder.
func (r *CoderAIProviderReconciler) check(
	ctx context.Context, provider *coderv1alpha1.CoderAIProvider, before *coderv1alpha1.CoderAIProviderStatus,
) (*aiProviderStep, error) {
	baseURL, err := aiProviderBaseURL(provider.Spec.BaseURL)
	if err != nil {
		// A fixed message: the parse error repeats the URL, which can hold
		// user info. A bad spec value stalls the object, not the reconcile.
		return aiProviderStalled(0, credentialsecret.ReasonMalformed, //nolint:nilerr // Reported in status.
			"spec.baseURL is not a URL that Coder accepts: it must be an http or https URL with a host, without user info, surrounding whitespace, or invalid percent escapes."), nil
	}
	key, err := credentialsecret.Read(ctx, r.Client, provider.Namespace, provider.Spec.APIKeySecretRef, baseURL)
	if credErr := (*credentialsecret.Error)(nil); errors.As(err, &credErr) {
		return aiProviderStalled(0, credErr.Reason, "%s", credErr.Message), nil
	} else if err != nil {
		return nil, err
	}
	sdk, _, wait, err := controlPlaneCoderClient(ctx, r.Client, provider.Namespace, provider.Spec.ControlPlaneRef.Name)
	if err != nil {
		return nil, err
	}
	if wait != nil {
		// The control plane watch ends most waits. A retry also covers an
		// operator token Secret that appears later.
		return aiProviderWait(aiProviderRetry, wait.reason, "%s", wait.message), nil
	}
	var step *aiProviderStep
	if provider.Status.ProviderID == "" {
		if step, err = r.createOrConfirm(ctx, sdk, provider, key, before); err != nil {
			return nil, err
		}
	}
	if step == nil {
		step = applyAIProvider(ctx, sdk, provider, key)
	}
	// Coder messages can echo the request, so scrub the key from them.
	step.message = credentialsecret.Redact(step.message, key)
	return step, nil
}

// aiProviderBaseURL parses the exact spec value the way Coder validates
// base_url (codersdk/aiproviders.go:459-476). It also requires a host name and
// refuses user info, as admission does, so the check holds without admission.
func aiProviderBaseURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) != raw {
		return nil, errors.New("leading or trailing whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if parsed.User != nil {
		return nil, errors.New("user info is not allowed")
	}
	if _, err := credentialsecret.Origin(parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// patchFinalizer adds or removes the cleanup finalizer with a merge patch. The
// optimistic lock turns a concurrent finalizer change into a conflict, not a
// lost update.
func (r *CoderAIProviderReconciler) patchFinalizer(ctx context.Context, provider *coderv1alpha1.CoderAIProvider, add bool) error {
	base := provider.DeepCopy()
	changed := controllerutil.RemoveFinalizer(provider, coderv1alpha1.CoderAIProviderCleanupFinalizer)
	if add {
		changed = controllerutil.AddFinalizer(provider, coderv1alpha1.CoderAIProviderCleanupFinalizer)
	}
	if !changed {
		return nil
	}
	if err := r.Patch(ctx, provider, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch finalizer of AI provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}
	return nil
}

// applyAIProviderStep sets the kstatus conditions for step. Messages are cut
// to the CoderTemplateTest limit.
func applyAIProviderStep(provider *coderv1alpha1.CoderAIProvider, step *aiProviderStep) {
	message := truncateTemplateTestMessage(step.message)
	ready, reconciling, stalled := metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionFalse
	switch {
	case step.ready:
		ready, reconciling = metav1.ConditionTrue, metav1.ConditionFalse
	case step.stalled:
		reconciling, stalled = metav1.ConditionFalse, metav1.ConditionTrue
	}
	for _, c := range []struct {
		conditionType string
		status        metav1.ConditionStatus
	}{
		{coderv1alpha1.CoderAIProviderConditionReady, ready},
		{coderv1alpha1.CoderAIProviderConditionReconciling, reconciling},
		{coderv1alpha1.CoderAIProviderConditionStalled, stalled},
	} {
		meta.SetStatusCondition(&provider.Status.Conditions, metav1.Condition{
			Type: c.conditionType, Status: c.status, ObservedGeneration: provider.Generation, Reason: step.reason, Message: message,
		})
	}
}

// writeStatus writes the status only when it changed.
func (r *CoderAIProviderReconciler) writeStatus(ctx context.Context, provider *coderv1alpha1.CoderAIProvider, before *coderv1alpha1.CoderAIProviderStatus) error {
	provider.Status.ObservedGeneration = provider.Generation
	if equality.Semantic.DeepEqual(before, &provider.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, provider); err != nil {
		return fmt.Errorf("update status of AI provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}
	return nil
}
