package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
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
	f := promoteFixture{state: state, storage: NewTemplatePromoteStorage(newTestClientProvider(t, server.URL))}
	for id, template := range state.templatesByID {
		f.templateID, f.v1 = id, template.ActiveVersionID
	}
	f.v2 = seedTemplateVersion(t, state, "v2").ID
	f.v3 = seedTemplateVersion(t, state, "v3").ID
	state.resetRequests()
	return f
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

func TestTemplatePromotePreview(t *testing.T) {
	t.Parallel()

	f := newPromoteFixture(t)
	_, updatedAt := f.active(t)

	got, err := f.promote(t, f.v2.String(), true)
	if err != nil {
		t.Fatalf("dry-run promote v2: %v", err)
	}
	requireResult(t, got, aggregationv1alpha1.PromotionResultWouldPromote, f.v1, f.v1)
	// Organization, template and version; no template source download.
	if requests := f.state.requests(); len(requests) != 3 || strings.Contains(strings.Join(requests, " "), "/files") {
		t.Fatalf("expected 3 Coder requests and no file download, got %v", requests)
	}
	for _, dryRun := range []bool{false, true} {
		got, err = f.promote(t, f.v1.String(), dryRun)
		if err != nil {
			t.Fatalf("promote the active version (dryRun=%v): %v", dryRun, err)
		}
		requireResult(t, got, aggregationv1alpha1.PromotionResultAlreadyActive, f.v1, f.v1)
	}
	// Until activation ships, a real request that needs a change is refused with a clear message.
	if _, err := f.promote(t, f.v2.String(), false); !apierrors.IsBadRequest(err) ||
		!strings.Contains(err.Error(), "promotion is not enabled yet; use dryRun=All to preview it") {
		t.Fatalf("expected the not-enabled 400, got %v", err)
	}

	active, updatedAtAfter := f.active(t)
	if active != f.v1 || !updatedAtAfter.Equal(updatedAt) {
		t.Fatalf("promote requests changed Coder: active=%s updatedAt %s -> %s", active, updatedAt, updatedAtAfter)
	}
	if mutations := f.state.mutations(); len(mutations) != 0 {
		t.Fatalf("promote requests must send no write yet, got %v", mutations)
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
