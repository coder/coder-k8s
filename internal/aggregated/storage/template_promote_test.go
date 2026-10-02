package storage

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
)

const promoteTemplateName = "acme.starter-template"

type promoteFixture struct {
	state      *mockCoderServerState
	storage    *TemplatePromoteStorage
	templateID uuid.UUID
	v1, v2, v3 uuid.UUID // v1 is active at the start
}

func newPromoteFixture(t *testing.T) promoteFixture {
	t.Helper()

	server, state := newMockCoderServer(t)
	t.Cleanup(server.Close)
	// The production client: the shared Coder transport reuses connections (#183), so the PATCH
	// travels on the connection that the lookups used.
	client, err := coder.NewSDKClient(coder.Config{CoderURL: mustParseURL(t, server.URL), SessionToken: "test-session-token"})
	if err != nil {
		t.Fatalf("build Coder client: %v", err)
	}
	f := promoteFixture{state: state, storage: NewTemplatePromoteStorage(&coder.StaticClientProvider{Client: client, Namespace: "control-plane"})}
	for id, template := range state.templatesByID {
		f.templateID, f.v1 = id, template.ActiveVersionID
	}
	f.v2 = seedTemplateVersion(t, state, "v2").ID
	f.v3 = seedTemplateVersion(t, state, "v3").ID
	state.resetRequests()
	return f
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}

func (f promoteFixture) promote(t *testing.T, versionID string, dryRun bool) (*aggregationv1alpha1.CoderTemplateVersionPromotion, error) {
	t.Helper()

	options := &metav1.CreateOptions{}
	if dryRun {
		options.DryRun = []string{metav1.DryRunAll}
	}
	request := &aggregationv1alpha1.CoderTemplateVersionPromotion{Spec: aggregationv1alpha1.CoderTemplateVersionPromotionSpec{VersionID: versionID}}
	obj, err := f.storage.Create(namespacedContext("control-plane"), promoteTemplateName, request, nil, options)
	if err != nil {
		return nil, err
	}
	return obj.(*aggregationv1alpha1.CoderTemplateVersionPromotion), nil
}

func (f promoteFixture) active(t *testing.T) (uuid.UUID, time.Time) {
	t.Helper()

	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	template := f.state.templatesByID[f.templateID]
	return template.ActiveVersionID, template.UpdatedAt
}

// setVersion edits a seeded version in the mock.
func (f promoteFixture) setVersion(id uuid.UUID, edit func(*codersdk.TemplateVersion)) {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	version := f.state.templateVersionsByID[id]
	edit(&version)
	f.state.templateVersionsByID[id] = version
}

func requireResult(t *testing.T, got *aggregationv1alpha1.CoderTemplateVersionPromotion, result aggregationv1alpha1.CoderTemplateVersionPromotionResult, previous, active uuid.UUID) {
	t.Helper()

	if got.Kind != "CoderTemplateVersionPromotion" || got.Name != promoteTemplateName || got.Namespace != "control-plane" {
		t.Fatalf("unexpected identity: kind=%q name=%q namespace=%q", got.Kind, got.Name, got.Namespace)
	}
	want := aggregationv1alpha1.CoderTemplateVersionPromotionStatus{Result: result, PreviousActiveVersionID: previous.String(), ActiveVersionID: active.String()}
	if got.Status != want {
		t.Fatalf("status = %+v, want %+v", got.Status, want)
	}
}

func TestTemplatePromoteOutcomes(t *testing.T) {
	t.Parallel()

	f := newPromoteFixture(t)
	patch := "PATCH /api/v2/templates/" + f.templateID.String() + "/versions"

	got, err := f.promote(t, f.v2.String(), false)
	if err != nil {
		t.Fatalf("promote v2: %v", err)
	}
	requireResult(t, got, aggregationv1alpha1.PromotionResultPromoted, f.v1, f.v2)
	if active, _ := f.active(t); active != f.v2 {
		t.Fatalf("Coder active version = %s, want v2", active)
	}
	// Organization, template, version, the PATCH and the confirming re-read; no template source.
	if requests := f.state.requests(); len(requests) != 5 || strings.Contains(strings.Join(requests, " "), "/files") {
		t.Fatalf("expected 5 Coder requests and no file download, got %v", requests)
	}

	got, err = f.promote(t, f.v1.String(), false)
	if err != nil {
		t.Fatalf("roll back to v1: %v", err)
	}
	requireResult(t, got, aggregationv1alpha1.PromotionResultPromoted, f.v2, f.v1)
	if mutations := f.state.mutations(); len(mutations) != 2 || mutations[0] != patch || mutations[1] != patch {
		t.Fatalf("expected exactly two PATCHes, got %v", mutations)
	}

	_, updatedAt := f.active(t)
	for _, dryRun := range []bool{false, true} {
		got, err = f.promote(t, f.v1.String(), dryRun)
		if err != nil {
			t.Fatalf("promote the active version (dryRun=%v): %v", dryRun, err)
		}
		requireResult(t, got, aggregationv1alpha1.PromotionResultAlreadyActive, f.v1, f.v1)
	}
	got, err = f.promote(t, f.v3.String(), true)
	if err != nil {
		t.Fatalf("dry-run promote v3: %v", err)
	}
	requireResult(t, got, aggregationv1alpha1.PromotionResultWouldPromote, f.v1, f.v1)

	active, updatedAtAfter := f.active(t)
	if active != f.v1 || !updatedAtAfter.Equal(updatedAt) {
		t.Fatalf("no-op and dry-run requests changed Coder: active=%s updatedAt %s -> %s", active, updatedAt, updatedAtAfter)
	}
	if mutations := f.state.mutations(); len(mutations) != 2 {
		t.Fatalf("no-op and dry-run requests must send no write, got %v", mutations)
	}
}

func TestTemplatePromoteRejectsBeforeAnyWrite(t *testing.T) {
	t.Parallel()

	otherTemplateID := uuid.New()
	cases := []struct {
		name      string
		setup     func(f promoteFixture) string // returns the version ID to promote
		template  string                        // request name; default promoteTemplateName
		body      string                        // metadata.name in the body
		check     func(error) bool
		message   string
		zeroCalls bool
	}{
		{name: "archived", setup: func(f promoteFixture) string {
			f.setVersion(f.v2, func(v *codersdk.TemplateVersion) { v.Archived = true })
			return f.v2.String()
		}, check: apierrors.IsBadRequest, message: "is archived"},
		{name: "pending job", setup: func(f promoteFixture) string {
			f.setVersion(f.v2, func(v *codersdk.TemplateVersion) { v.Job.Status = codersdk.ProvisionerJobPending })
			return f.v2.String()
		}, check: apierrors.IsBadRequest, message: `its import job is "pending"`},
		{name: "failed job", setup: func(f promoteFixture) string {
			f.setVersion(f.v2, func(v *codersdk.TemplateVersion) { v.Job.Status = codersdk.ProvisionerJobFailed })
			return f.v2.String()
		}, check: apierrors.IsBadRequest, message: `its import job is "failed"`},
		{
			name: "unknown version", setup: func(promoteFixture) string { return uuid.NewString() },
			check: apierrors.IsBadRequest, message: `spec.versionID is not a version of template "acme.starter-template"`,
		},
		{name: "version of another template", setup: func(f promoteFixture) string {
			f.setVersion(f.v2, func(v *codersdk.TemplateVersion) { v.TemplateID = &otherTemplateID })
			return f.v2.String()
		}, check: apierrors.IsBadRequest, message: `spec.versionID is not a version of template "acme.starter-template"`},
		{name: "orphan version", setup: func(f promoteFixture) string {
			f.setVersion(f.v2, func(v *codersdk.TemplateVersion) { v.TemplateID = nil })
			return f.v2.String()
		}, check: apierrors.IsBadRequest, message: `spec.versionID is not a version of template "acme.starter-template"`},
		{name: "organization contradiction fails closed", setup: func(f promoteFixture) string {
			f.setVersion(f.v2, func(v *codersdk.TemplateVersion) { v.OrganizationID = uuid.New() })
			return f.v2.String()
		}, check: isPlainError, message: "assertion failed"},
		{name: "not a UUID", setup: func(promoteFixture) string { return "v2" }, check: apierrors.IsInvalid, message: "spec.versionID", zeroCalls: true},
		{name: "empty version ID", setup: func(promoteFixture) string { return "" }, check: apierrors.IsInvalid, message: "spec.versionID", zeroCalls: true},
		{
			name: "body name mismatch", setup: func(f promoteFixture) string { return f.v2.String() }, body: "acme.other",
			check: apierrors.IsBadRequest, message: "must be empty or match", zeroCalls: true,
		},
		{
			name: "not a template name", setup: func(f promoteFixture) string { return f.v2.String() }, template: "starter-template",
			check: apierrors.IsBadRequest, message: "invalid template name", zeroCalls: true,
		},
		{
			name: "organization alias", setup: func(f promoteFixture) string { return f.v2.String() },
			template: "ORG_ID.starter-template", check: apierrors.IsBadRequest, message: "use the canonical organization name",
		},
		{name: "template casing", setup: func(f promoteFixture) string {
			f.state.mu.Lock()
			f.state.caseInsensitiveLeafLookups = true
			f.state.mu.Unlock()
			return f.v2.String()
		}, template: "acme.Starter-Template", check: apierrors.IsBadRequest, message: `use the canonical name "acme.starter-template"`},
		{
			name: "query in template segment", setup: func(f promoteFixture) string { return f.v2.String() },
			template: "acme.starter-template?x=1", check: apierrors.IsBadRequest, message: "resolves to template",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newPromoteFixture(t)
			versionID := tc.setup(f)
			name := promoteTemplateName
			if tc.template != "" {
				name = strings.ReplaceAll(tc.template, "ORG_ID", f.state.organization.ID.String())
			}
			request := &aggregationv1alpha1.CoderTemplateVersionPromotion{
				ObjectMeta: metav1.ObjectMeta{Name: tc.body},
				Spec:       aggregationv1alpha1.CoderTemplateVersionPromotionSpec{VersionID: versionID},
			}
			_, err := f.storage.Create(namespacedContext("control-plane"), name, request, nil, &metav1.CreateOptions{})
			if err == nil || !tc.check(err) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("expected %s error containing %q, got %v", tc.name, tc.message, err)
			}
			if mutations := f.state.mutations(); len(mutations) != 0 {
				t.Fatalf("rejected promotion reached a Coder write: %v", mutations)
			}
			if requests := f.state.requests(); tc.zeroCalls && len(requests) != 0 {
				t.Fatalf("expected no Coder request, got %v", requests)
			}
		})
	}
}

// isPlainError reports an error that is not a Kubernetes status; the API server answers it with 500.
func isPlainError(err error) bool {
	var status apierrors.APIStatus
	return !errors.As(err, &status)
}

func TestTemplatePromoteAdmissionRunsBeforeCoder(t *testing.T) {
	t.Parallel()

	for _, dryRun := range []string{"", metav1.DryRunAll} {
		f := newPromoteFixture(t)
		options := &metav1.CreateOptions{}
		if dryRun != "" {
			options.DryRun = []string{dryRun}
		}
		denied := errors.New("denied by admission")
		request := &aggregationv1alpha1.CoderTemplateVersionPromotion{Spec: aggregationv1alpha1.CoderTemplateVersionPromotionSpec{VersionID: f.v2.String()}}
		_, err := f.storage.Create(namespacedContext("control-plane"), promoteTemplateName, request,
			func(context.Context, runtime.Object) error { return denied }, options)
		if !errors.Is(err, denied) {
			t.Fatalf("dryRun=%q: expected the admission error, got %v", dryRun, err)
		}
		if requests := f.state.requests(); len(requests) != 0 {
			t.Fatalf("dryRun=%q: admission denial reached Coder: %v", dryRun, requests)
		}
	}
}

// TestTemplatePromoteConfirmation covers every row of the confirmation table: an activation is
// reported as Promoted only when the re-read shows the target, and never retried.
func TestTemplatePromoteConfirmation(t *testing.T) {
	t.Parallel()

	const third, previous = "third", "previous"
	cases := []struct {
		name          string
		fault         promoteFault
		activateAfter string
		patchBudget   time.Duration
		promoted      bool
		check         func(error) bool
		message       string
		noReread      bool
	}{
		{name: "dropped after apply", fault: promoteFault{apply: true}, promoted: true},
		{name: "5xx after apply", fault: promoteFault{apply: true, status: http.StatusBadGateway}, promoted: true},
		{name: "timeout after apply", fault: promoteFault{apply: true, delay: 2 * time.Second}, patchBudget: 50 * time.Millisecond, promoted: true},
		{name: "dropped before apply", fault: promoteFault{}, check: apierrors.IsServiceUnavailable, message: "could not confirm"},
		{name: "5xx before apply", fault: promoteFault{status: http.StatusInternalServerError}, check: apierrors.IsServiceUnavailable, message: "could not confirm"},
		{
			name: "timeout before apply", fault: promoteFault{delay: 2 * time.Second}, patchBudget: 50 * time.Millisecond,
			check: apierrors.IsServiceUnavailable, message: "could not confirm",
		},
		{
			name: "200 then superseded", fault: promoteFault{apply: true, status: http.StatusOK}, activateAfter: third,
			check: apierrors.IsConflict, message: "superseded by a concurrent change",
		},
		{
			name: "200 then reverted", fault: promoteFault{apply: true, status: http.StatusOK}, activateAfter: previous,
			check: apierrors.IsConflict, message: "superseded by a concurrent change",
		},
		{
			name: "ambiguous and a third version", fault: promoteFault{}, activateAfter: third,
			check: apierrors.IsConflict, message: "superseded by a concurrent change",
		},
		{
			name: "re-read fails", fault: promoteFault{apply: true, status: http.StatusOK, failReread: true},
			check: apierrors.IsServiceUnavailable, message: "could not confirm",
		},
		{name: "Coder rate limit", fault: promoteFault{status: http.StatusTooManyRequests}, check: apierrors.IsTooManyRequests, noReread: true},
		{
			name: "Coder 404: changed meanwhile", fault: promoteFault{status: http.StatusNotFound}, check: apierrors.IsConflict,
			message: "was not found in Coder during the activation", noReread: true,
		},
		{
			name: "Coder 403 is not an RBAC denial", fault: promoteFault{status: http.StatusForbidden}, check: apierrors.IsBadRequest,
			message: "Coder refused to activate", noReread: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newPromoteFixture(t)
			if tc.patchBudget > 0 {
				f.storage.patchBudget = tc.patchBudget
			}
			fault := tc.fault
			switch tc.activateAfter {
			case third:
				fault.activateAfter = f.v3
			case previous:
				fault.activateAfter = f.v1
			}
			f.state.mu.Lock()
			f.state.promoteFault = &fault
			f.state.mu.Unlock()

			start := time.Now()
			got, err := f.promote(t, f.v2.String(), false)
			// A slow activation is cut at the PATCH budget, so the re-read still fits the request deadline.
			if elapsed := time.Since(start); tc.patchBudget > 0 && elapsed > time.Second {
				t.Fatalf("promotion waited %s for a slow activation; the PATCH budget is %s", elapsed, tc.patchBudget)
			}
			if tc.promoted {
				if err != nil {
					t.Fatalf("expected Promoted, got %v", err)
				}
				requireResult(t, got, aggregationv1alpha1.PromotionResultPromoted, f.v1, f.v2)
			} else if err == nil || !tc.check(err) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("expected an error containing %q, got %v (result %+v)", tc.message, err, got)
			}
			// Error messages are fixed: no Coder error text and no Coder URL.
			if err != nil && (strings.Contains(err.Error(), "injected") || strings.Contains(err.Error(), "127.0.0.1")) {
				t.Fatalf("error passes on Coder text or the Coder URL: %v", err)
			}
			var status apierrors.APIStatus
			if errors.As(err, &status) && status.Status().Details != nil && status.Status().Details.RetryAfterSeconds != 0 {
				t.Fatalf("an uncertain promotion must not invite an automatic retry: %+v", status.Status())
			}

			if mutations := f.state.mutations(); len(mutations) != 1 {
				t.Fatalf("expected exactly one PATCH and no retry, got %v", mutations)
			}
			reread := "GET /api/v2/templates/" + f.templateID.String()
			if hasReread := strings.Contains(strings.Join(f.state.requests(), "\n")+"\n", reread+"\n"); hasReread == tc.noReread {
				t.Fatalf("confirming re-read sent = %v, want %v: %v", hasReread, !tc.noReread, f.state.requests())
			}
		})
	}
}

func TestTemplatePromoteEmitsNoTemplateWatchEvent(t *testing.T) {
	t.Parallel()

	f := newPromoteFixture(t)
	templates := NewTemplateStorage(f.storage.provider)
	t.Cleanup(templates.Destroy)
	watcher, err := templates.Watch(namespacedContext("control-plane"), nil)
	if err != nil {
		t.Fatalf("watch templates: %v", err)
	}
	defer watcher.Stop()

	if _, err := f.promote(t, f.v2.String(), false); err != nil {
		t.Fatalf("promote v2: %v", err)
	}
	select {
	case event := <-watcher.ResultChan():
		t.Fatalf("promotion emitted a codertemplates watch event: %+v", event)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestTemplatePromoteDeadline: the activation is sent only if it and the confirming re-read fit the
// request deadline, and the re-read ends before the deadline, so the storage answers before the API
// server's own 504.
func TestTemplatePromoteDeadline(t *testing.T) {
	t.Parallel()

	t.Run("too little time left: 504 and nothing sent", func(t *testing.T) {
		t.Parallel()
		f := newPromoteFixture(t)
		ctx, cancel := context.WithTimeout(namespacedContext("control-plane"), TemplatePromotePatchBudget+TemplatePromoteRereadReserve-time.Second)
		defer cancel()
		request := &aggregationv1alpha1.CoderTemplateVersionPromotion{Spec: aggregationv1alpha1.CoderTemplateVersionPromotionSpec{VersionID: f.v2.String()}}
		_, err := f.storage.Create(ctx, promoteTemplateName, request, nil, &metav1.CreateOptions{})
		if !apierrors.IsTimeout(err) || !strings.Contains(err.Error(), "nothing was sent to Coder") {
			t.Fatalf("expected the fixed not-attempted 504, got %v", err)
		}
		if mutations := f.state.mutations(); len(mutations) != 0 {
			t.Fatalf("a promotion without enough time left wrote to Coder: %v", mutations)
		}
	})

	t.Run("slow re-read ends before the deadline", func(t *testing.T) {
		t.Parallel()
		f := newPromoteFixture(t)
		f.storage.patchBudget, f.storage.rereadReserve = 100*time.Millisecond, time.Second
		f.state.mu.Lock()
		f.state.promoteFault = &promoteFault{apply: true, status: http.StatusOK, rereadDelay: 3 * time.Second}
		f.state.mu.Unlock()
		ctx, cancel := context.WithTimeout(namespacedContext("control-plane"), 1500*time.Millisecond)
		defer cancel()
		deadline, _ := ctx.Deadline()
		request := &aggregationv1alpha1.CoderTemplateVersionPromotion{Spec: aggregationv1alpha1.CoderTemplateVersionPromotionSpec{VersionID: f.v2.String()}}
		_, err := f.storage.Create(ctx, promoteTemplateName, request, nil, &metav1.CreateOptions{})
		if !apierrors.IsServiceUnavailable(err) || !strings.Contains(err.Error(), "could not confirm") {
			t.Fatalf("expected 503 could-not-confirm, got %v", err)
		}
		if !time.Now().Before(deadline) {
			t.Fatal("the storage answered after the request deadline, so the API server would have answered 504 first")
		}
		if mutations := f.state.mutations(); len(mutations) != 1 {
			t.Fatalf("expected exactly one PATCH, got %v", mutations)
		}
	})
}
