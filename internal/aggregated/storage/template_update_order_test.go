package storage

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
)

// templateUpdateMeta is the Coder-side template metadata that a CoderTemplate Update can change.
type templateUpdateMeta struct {
	displayName string
	description string
	icon        string
}

func (s *mockCoderServerState) templateByName(organization, templateName string) (codersdk.Template, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	templateID, ok := s.templateIDsByOrg[organization][templateName]
	if !ok {
		return codersdk.Template{}, false
	}
	template, ok := s.templatesByID[templateID]
	return template, ok
}

func (s *mockCoderServerState) templateMetaByName(t *testing.T, organization, templateName string) templateUpdateMeta {
	t.Helper()
	template, ok := s.templateByName(organization, templateName)
	if !ok {
		t.Fatalf("expected template %s/%s in the mock", organization, templateName)
	}
	return templateUpdateMeta{displayName: template.DisplayName, description: template.Description, icon: template.Icon}
}

// changeTemplateOutOfBand models another client changing the template's metadata in Coder (new UpdatedAt).
func (s *mockCoderServerState) changeTemplateOutOfBand(organization, templateName, displayName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	templateID, ok := s.templateIDsByOrg[organization][templateName]
	if !ok {
		return false
	}
	template := s.templatesByID[templateID]
	template.DisplayName = displayName
	template.UpdatedAt = template.UpdatedAt.Add(time.Second)
	s.templatesByID[templateID] = template
	return true
}

// replaceTemplateOutOfBand models another client deleting the template and creating a new one with the same
// name. The replacement keeps UpdatedAt, so only its UID differs from the original.
func (s *mockCoderServerState) replaceTemplateOutOfBand(organization, templateName, displayName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldID, ok := s.templateIDsByOrg[organization][templateName]
	if !ok {
		return false
	}
	replacement := s.templatesByID[oldID]
	delete(s.templatesByID, oldID)
	replacement.ID = uuid.New()
	replacement.DisplayName = displayName
	s.templatesByID[replacement.ID] = replacement
	s.templateIDsByOrg[organization][templateName] = replacement.ID
	return true
}

// templateMetaPatches returns the indexes of UpdateTemplateMeta requests (PATCH /api/v2/templates/{id}).
func templateMetaPatches(mutations []string) []int {
	var indexes []int
	for i, mutation := range mutations {
		path, ok := strings.CutPrefix(mutation, "PATCH /api/v2/templates/")
		if ok && !strings.Contains(path, "/") {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

// activeVersionPromotions returns the indexes of UpdateActiveTemplateVersion requests (PATCH /api/v2/templates/{id}/versions).
func activeVersionPromotions(mutations []string) []int {
	var indexes []int
	for i, mutation := range mutations {
		if strings.HasPrefix(mutation, "PATCH /api/v2/templates/") && strings.HasSuffix(mutation, "/versions") {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

// createTemplateForMetadataUpdate creates acme.<leafName> with files and returns it with the metadata it
// holds in Coder.
func createTemplateForMetadataUpdate(
	t *testing.T,
	templateStorage *TemplateStorage,
	state *mockCoderServerState,
	leafName string,
) (*aggregationv1alpha1.CoderTemplate, templateUpdateMeta) {
	t.Helper()

	createObj := filesTemplateForCreate(leafName)
	createObj.Spec.DisplayName = "Original Name"
	createObj.Spec.Description = "original description"
	createObj.Spec.Icon = "/icons/original.png"
	createdObj, err := templateStorage.Create(namespacedContext("control-plane"), createObj, rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatalf("create template %s: %v", leafName, err)
	}
	created, ok := createdObj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		t.Fatalf("expected *CoderTemplate from create, got %T", createdObj)
	}
	meta := state.templateMetaByName(t, "acme", leafName)
	if meta.displayName != "Original Name" {
		t.Fatalf("assertion failed: expected the created template to hold the original metadata, got %+v", meta)
	}
	return created, meta
}

// metadataAndFilesUpdate changes every metadata field and the source.
func metadataAndFilesUpdate(current *aggregationv1alpha1.CoderTemplate) *aggregationv1alpha1.CoderTemplate {
	desired := current.DeepCopy()
	desired.Spec.DisplayName = "Renamed With New Source"
	desired.Spec.Description = "new description"
	desired.Spec.Icon = "/icons/new.png"
	desired.Spec.Files = map[string]string{"main.tf": `resource "null_resource" "metadata_order_updated" {}`}
	return desired
}

func updateTemplate(
	ctx context.Context,
	templateStorage *TemplateStorage,
	desired *aggregationv1alpha1.CoderTemplate,
) (*aggregationv1alpha1.CoderTemplate, error) {
	updatedObj, created, err := templateStorage.Update(
		ctx,
		desired.Name,
		testUpdatedObjectInfo{obj: desired},
		nil,
		rest.ValidateAllObjectUpdateFunc,
		false,
		nil,
	)
	if created {
		panic("assertion failed: CoderTemplate update must never report created=true for an existing template")
	}
	if err != nil {
		if updatedObj != nil {
			panic("assertion failed: a failed update must not return an object")
		}
		return nil, err
	}
	updated, ok := updatedObj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		panic("assertion failed: update must return *CoderTemplate")
	}
	return updated, nil
}

// When the new source does not become active, an Update that also changes metadata must leave the
// template exactly as it was (#117): no metadata write, no promotion.
func TestTemplateStorageUpdateLeavesMetadataUnchangedWhenNewSourceIsNotActivated(t *testing.T) {
	pending := func(state *mockCoderServerState) {
		state.setNextCreatedTemplateVersionStatus(codersdk.ProvisionerJobPending)
	}
	tests := []struct {
		name        string
		waitTimeout string
		setup       func(state *mockCoderServerState)
		requestCtx  func(t *testing.T) context.Context
		wantErr     func(error) bool
		wantMessage string
	}{
		{
			name: "failed import",
			setup: func(state *mockCoderServerState) {
				state.setNextCreatedTemplateVersionStatus(codersdk.ProvisionerJobFailed)
			},
			wantErr:     apierrors.IsBadRequest,
			wantMessage: "build ended with status",
		},
		{
			name: "canceled import",
			setup: func(state *mockCoderServerState) {
				state.setNextCreatedTemplateVersionStatus(codersdk.ProvisionerJobCanceled)
			},
			wantErr:     apierrors.IsBadRequest,
			wantMessage: "build ended with status",
		},
		{name: "configured wait timeout", waitTimeout: "120ms", setup: pending, wantErr: apierrors.IsBadRequest, wantMessage: "did not succeed within"},
		{
			name:  "request deadline",
			setup: pending,
			requestCtx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(namespacedContext("control-plane"), 300*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
			wantErr:     apierrors.IsTimeout,
			wantMessage: "did not succeed within",
		},
		{
			name:  "request cancellation",
			setup: pending,
			requestCtx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(namespacedContext("control-plane"))
				time.AfterFunc(300*time.Millisecond, cancel)
				t.Cleanup(cancel)
				return ctx
			},
			wantErr:     apierrors.IsTimeout,
			wantMessage: "did not succeed within",
		},
		{
			name:        "promotion does not take effect",
			setup:       func(state *mockCoderServerState) { state.setFailActiveVersionPromotion(true) },
			wantErr:     func(err error) bool { return err != nil },
			wantMessage: "active version promotion did not take effect",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			waitTimeout := tc.waitTimeout
			if waitTimeout == "" {
				waitTimeout = "5s"
			}
			setFastTemplateVersionBuildPolling(t, waitTimeout)

			templateStorage, state, _ := newTemplateCreateWaitHarness(t, nil)
			current, metaBefore := createTemplateForMetadataUpdate(t, templateStorage, state, "meta-order")
			activeBefore, ok := state.templateActiveVersionID("acme", "meta-order")
			if !ok {
				t.Fatal("expected an active version before the update")
			}
			metaCallsBefore := state.templateMetaUpdateCount()
			versionsBefore := state.templateVersionCount()
			tc.setup(state)

			ctx := namespacedContext("control-plane")
			if tc.requestCtx != nil {
				ctx = tc.requestCtx(t)
			}
			_, err := updateTemplate(ctx, templateStorage, metadataAndFilesUpdate(current))
			if err == nil || !tc.wantErr(err) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("expected an update error containing %q, got %T: %v", tc.wantMessage, err, err)
			}

			// Nothing may be applied late either, after a timeout or cancellation.
			time.Sleep(200 * time.Millisecond)
			if versions := state.templateVersionCount(); versions != versionsBefore+1 {
				t.Fatalf("assertion failed: expected the update to create one template version, before=%d after=%d", versionsBefore, versions)
			}
			if metaAfter := state.templateMetaByName(t, "acme", "meta-order"); metaAfter != metaBefore {
				t.Fatalf("expected template metadata to stay unchanged when the new source is not activated, before=%+v after=%+v", metaBefore, metaAfter)
			}
			if calls := state.templateMetaUpdateCount(); calls != metaCallsBefore {
				t.Fatalf("expected no metadata update call, before=%d after=%d", metaCallsBefore, calls)
			}
			if activeAfter, _ := state.templateActiveVersionID("acme", "meta-order"); activeAfter != activeBefore {
				t.Fatalf("expected the active version to stay %s, got %s", activeBefore, activeAfter)
			}
		})
	}
}

func TestTemplateStorageUpdateAppliesMetadataAfterNewSourceIsActive(t *testing.T) {
	setFastTemplateVersionBuildPolling(t, "5s")

	templateStorage, state, _ := newTemplateCreateWaitHarness(t, nil)
	current, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "meta-applied")
	activeBefore, ok := state.templateActiveVersionID("acme", "meta-applied")
	if !ok {
		t.Fatal("expected an active version before the update")
	}
	mutationsBefore := len(state.mutations())
	state.setNextCreatedTemplateVersionPendingForPolls(2)

	desired := metadataAndFilesUpdate(current)
	updated, err := updateTemplate(namespacedContext("control-plane"), templateStorage, desired)
	if err != nil {
		t.Fatalf("expected the update to succeed after the import succeeded: %v", err)
	}

	wantMeta := templateUpdateMeta{displayName: desired.Spec.DisplayName, description: desired.Spec.Description, icon: desired.Spec.Icon}
	if metaAfter := state.templateMetaByName(t, "acme", "meta-applied"); metaAfter != wantMeta {
		t.Fatalf("expected metadata %+v in Coder, got %+v", wantMeta, metaAfter)
	}
	activeAfter, _ := state.templateActiveVersionID("acme", "meta-applied")
	if activeAfter == activeBefore {
		t.Fatalf("expected a new active version, still %s", activeAfter)
	}
	if updated.Spec.DisplayName != wantMeta.displayName || updated.Spec.Description != wantMeta.description || updated.Spec.Icon != wantMeta.icon {
		t.Fatalf("expected the returned object to carry the new metadata, got %+v", updated.Spec)
	}
	if !strings.Contains(updated.Spec.Files["main.tf"], "metadata_order_updated") {
		t.Fatalf("expected the returned object to carry the new source, got %v", updated.Spec.Files)
	}

	mutations := state.mutations()[mutationsBefore:]
	metaPatches, promotions := templateMetaPatches(mutations), activeVersionPromotions(mutations)
	if len(metaPatches) != 1 || len(promotions) != 1 {
		t.Fatalf("expected one metadata update and one promotion, got %v", mutations)
	}
	if metaPatches[0] < promotions[0] {
		t.Fatalf("expected the metadata update after the promotion, got %v", mutations)
	}
}

// A template changed or replaced in Coder while the new source was importing must not be overwritten:
// the Update returns 409 Conflict before promoting the version or writing metadata.
func TestTemplateStorageUpdateReturnsConflictWhenTemplateChangesDuringImport(t *testing.T) {
	tests := []struct {
		name   string
		change func(state *mockCoderServerState) bool
	}{
		{
			name: "resourceVersion changed",
			change: func(state *mockCoderServerState) bool {
				return state.changeTemplateOutOfBand("acme", "meta-conflict", "Changed Elsewhere")
			},
		},
		{
			name: "template replaced (UID changed)",
			change: func(state *mockCoderServerState) bool {
				return state.replaceTemplateOutOfBand("acme", "meta-conflict", "Changed Elsewhere")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setFastTemplateVersionBuildPolling(t, "5s")

			var (
				armed        sync.Mutex
				activeBefore uuid.UUID
				state        *mockCoderServerState
				changed      = make(chan struct{})
				changeOnce   sync.Once
			)
			armed.Lock()
			templateStorage, harnessState, _ := newTemplateCreateWaitHarness(t, func(_ http.ResponseWriter, r *http.Request) bool {
				if !isTemplateVersionPoll(r) || !armed.TryLock() {
					return false
				}
				defer armed.Unlock()
				// Change the template on the first poll of the new version, i.e. while the import runs.
				if strings.HasSuffix(r.URL.Path, "/"+activeBefore.String()) {
					return false
				}
				changeOnce.Do(func() {
					if !tc.change(state) {
						t.Error("assertion failed: out-of-band change found no template")
					}
					close(changed)
				})
				return false
			})
			state = harnessState

			current, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "meta-conflict")
			var ok bool
			activeBefore, ok = state.templateActiveVersionID("acme", "meta-conflict")
			if !ok {
				t.Fatal("expected an active version before the update")
			}
			state.setNextCreatedTemplateVersionPendingForPolls(2)
			mutationsBefore := len(state.mutations())
			armed.Unlock()

			_, err := updateTemplate(namespacedContext("control-plane"), templateStorage, metadataAndFilesUpdate(current))
			select {
			case <-changed:
			default:
				t.Fatal("assertion failed: the template was not changed during the import")
			}
			if !apierrors.IsConflict(err) {
				t.Fatalf("expected 409 Conflict after the template changed during the import, got %T: %v", err, err)
			}
			assertTopLevelStatusError(t, err)
			if !strings.Contains(err.Error(), "while the new source was importing") {
				t.Fatalf("expected the conflict to explain the concurrent change, got %v", err)
			}

			mutations := state.mutations()[mutationsBefore:]
			if metaPatches, promotions := templateMetaPatches(mutations), activeVersionPromotions(mutations); len(metaPatches)+len(promotions) != 0 {
				t.Fatalf("expected no promotion and no metadata write after the conflict, got %v", mutations)
			}
			if !slices.ContainsFunc(mutations, func(m string) bool { return strings.HasSuffix(m, "/templateversions") }) {
				t.Fatalf("assertion failed: expected the update to have created a template version, got %v", mutations)
			}
			template, ok := state.templateByName("acme", "meta-conflict")
			if !ok || template.DisplayName != "Changed Elsewhere" || template.ActiveVersionID != activeBefore {
				t.Fatalf("expected the concurrently changed template to stay untouched, got ok=%t name=%q active=%s", ok, template.DisplayName, template.ActiveVersionID)
			}
		})
	}
}
