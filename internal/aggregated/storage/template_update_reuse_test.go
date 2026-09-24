package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
)

// newTemplateReuseHarness is newTemplateCreateWaitHarness without the watch, returning the mock URL so a test
// can build a second storage (a restarted server) against the same Coder.
func newTemplateReuseHarness(
	t *testing.T,
	hook func(w http.ResponseWriter, r *http.Request) bool,
) (*TemplateStorage, *mockCoderServerState, string) {
	t.Helper()

	mock, state := newMockCoderServer(t)
	t.Cleanup(mock.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil && hook(w, r) {
			return
		}
		state.handleRequest(t, w, r)
	}))
	t.Cleanup(server.Close)

	templateStorage := NewTemplateStorage(newTestClientProvider(t, server.URL))
	t.Cleanup(templateStorage.Destroy)
	return templateStorage, state, server.URL
}

func isCreateTemplateVersion(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v2/organizations/") &&
		strings.HasSuffix(r.URL.Path, "/templateversions")
}

func isTemplateVersionByNameLookup(r *http.Request) bool {
	segments := splitPath(r.URL.Path)
	return r.Method == http.MethodGet && hasSegments(segments, "api", "v2", "templates") && len(segments) == 6 &&
		segments[4] == "versions"
}

func isActiveVersionPromotion(r *http.Request) bool {
	return r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v2/templates/") &&
		strings.HasSuffix(r.URL.Path, "/versions")
}

func countMutations(state *mockCoderServerState, from int, match func(string) bool) int {
	count := 0
	for _, mutation := range state.mutations()[from:] {
		if match(mutation) {
			count++
		}
	}
	return count
}

func createTemplateVersionMutation(mutation string) bool {
	return strings.HasPrefix(mutation, "POST /api/v2/organizations/") && strings.HasSuffix(mutation, "/templateversions")
}

func uploadMutation(mutation string) bool { return mutation == "POST /api/v2/files" }

// templateVersionsOf returns the versions bound to organization/templateName, oldest first.
func (s *mockCoderServerState) templateVersionsOf(t *testing.T, organization, templateName string) []codersdk.TemplateVersion {
	t.Helper()
	template, ok := s.templateByName(organization, templateName)
	if !ok {
		t.Fatalf("expected template %s/%s in the mock", organization, templateName)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var versions []codersdk.TemplateVersion
	for _, version := range s.templateVersionsByID {
		if version.TemplateID != nil && *version.TemplateID == template.ID {
			versions = append(versions, version)
		}
	}
	sort.Slice(versions, func(i, j int) bool {
		if !versions[i].CreatedAt.Equal(versions[j].CreatedAt) {
			return versions[i].CreatedAt.Before(versions[j].CreatedAt)
		}
		return versions[i].Name < versions[j].Name
	})
	return versions
}

func (s *mockCoderServerState) setTemplateVersionState(id uuid.UUID, status codersdk.ProvisionerJobStatus, archived bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	version, ok := s.templateVersionsByID[id]
	if !ok {
		panic("assertion failed: template version to update must exist in the mock")
	}
	version.Job.Status = status
	version.Archived = archived
	delete(s.templateVersionPollsBeforeSuccess, id)
	s.templateVersionsByID[id] = version
}

// seedTemplateVersion adds a version as if another request had created it; pendingPolls > 0 keeps it
// pending for that many by-ID polls.
func (s *mockCoderServerState) seedTemplateVersion(
	templateID uuid.UUID,
	name string,
	fileID uuid.UUID,
	status codersdk.ProvisionerJobStatus,
	pendingPolls int,
) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()

	templateIDCopy := templateID
	now := time.Now().UTC()
	version := codersdk.TemplateVersion{
		ID:             uuid.New(),
		TemplateID:     &templateIDCopy,
		OrganizationID: s.organization.ID,
		CreatedAt:      now,
		UpdatedAt:      now,
		Name:           name,
		Job:            codersdk.ProvisionerJob{FileID: fileID, Status: status},
	}
	s.templateVersionsByID[version.ID] = version
	if pendingPolls > 0 {
		s.templateVersionPollsBeforeSuccess[version.ID] = pendingPolls
	}
	return version.ID
}

func filesUpdate(current *aggregationv1alpha1.CoderTemplate, resourceName string) *aggregationv1alpha1.CoderTemplate {
	desired := current.DeepCopy()
	desired.Spec.Files = map[string]string{"main.tf": `resource "null_resource" "` + resourceName + `" {}`}
	return desired
}

func requestTimeoutContext(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(namespacedContext("control-plane"), timeout)
	t.Cleanup(cancel)
	return ctx
}

var managedTemplateVersionName = regexp.MustCompile(`^k8s-[0-9a-f]{20}(-[0-9]+)?$`)

// newestVersion returns the newest version bound to the template (the mock leaves a Create's first version
// unbound, so only Update attempts count) and checks its derived name.
func newestVersion(t *testing.T, state *mockCoderServerState, templateName string, wantCount int) codersdk.TemplateVersion {
	t.Helper()
	versions := state.templateVersionsOf(t, "acme", templateName)
	if len(versions) != wantCount {
		t.Fatalf("expected %d template versions for %s, got %d: %+v", wantCount, templateName, len(versions), versions)
	}
	newest := versions[len(versions)-1]
	if !managedTemplateVersionName.MatchString(newest.Name) {
		t.Fatalf("expected a derived k8s-<hex20> version name, got %q", newest.Name)
	}
	return newest
}

func TestTemplateVersionBaseName(t *testing.T) {
	templateA, templateB := uuid.New(), uuid.New()
	zip1, zip2 := []byte("zip-one"), []byte("zip-two")

	nameA1, err := templateVersionBaseName(templateA, zip1)
	if err != nil {
		t.Fatalf("derive name: %v", err)
	}
	if again, _ := templateVersionBaseName(templateA, zip1); again != nameA1 {
		t.Fatalf("expected a stable name, got %q then %q", nameA1, again)
	}
	if !regexp.MustCompile(`^k8s-[0-9a-f]{20}$`).MatchString(nameA1) {
		t.Fatalf("expected k8s-<hex20>, got %q", nameA1)
	}
	if nameB1, _ := templateVersionBaseName(templateB, zip1); nameB1 == nameA1 {
		t.Fatal("expected different templates with the same source to get different names")
	}
	if nameA2, _ := templateVersionBaseName(templateA, zip2); nameA2 == nameA1 {
		t.Fatal("expected different sources for one template to get different names")
	}
	if _, err := templateVersionBaseName(uuid.Nil, zip1); err == nil || !strings.Contains(err.Error(), "assertion failed") {
		t.Fatalf("expected an assertion for a nil template ID, got %v", err)
	}
	if _, err := templateVersionBaseName(templateA, nil); err == nil || !strings.Contains(err.Error(), "assertion failed") {
		t.Fatalf("expected an assertion for an empty zip, got %v", err)
	}

	for attempt, want := range map[int]string{1: nameA1, 2: nameA1 + "-2", 137: nameA1 + "-137"} {
		got, err := templateVersionAttemptName(nameA1, attempt)
		if err != nil || got != want {
			t.Fatalf("attempt %d: expected %q, got %q (err %v)", attempt, want, got, err)
		}
		if err := codersdk.TemplateVersionNameValid(got); err != nil {
			t.Fatalf("attempt %d: Coder would reject %q: %v", attempt, got, err)
		}
	}
	if _, err := templateVersionAttemptName(nameA1, 0); err == nil || !strings.Contains(err.Error(), "assertion failed") {
		t.Fatalf("expected an assertion for attempt 0, got %v", err)
	}
}

// A retry after a timeout must wait on the version the first request started instead of importing again,
// also when the retry reaches a restarted server (the name is recomputed, nothing is kept in memory).
func TestTemplateStorageUpdateRetryReusesPendingVersion(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "same server"
		if restart {
			name = "restarted server"
		}
		t.Run(name, func(t *testing.T) {
			setFastTemplateVersionBuildPolling(t, "5s")
			templateStorage, state, serverURL := newTemplateReuseHarness(t, nil)
			current, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "reuse-pending")
			mutationsBefore := len(state.mutations())

			state.setNextCreatedTemplateVersionPendingForPolls(60) // ~0.6 s of 10 ms polls: outlives the first request
			desired := filesUpdate(current, "reuse_pending")
			if _, err := updateTemplate(requestTimeoutContext(t, 250*time.Millisecond), templateStorage, desired); !apierrors.IsTimeout(err) {
				t.Fatalf("expected the first update to time out while the import is pending, got %v", err)
			}
			started := newestVersion(t, state, "reuse-pending", 1)
			if started.Job.Status != codersdk.ProvisionerJobPending {
				t.Fatalf("assertion failed: expected the first attempt to still be pending, got %s", started.Job.Status)
			}

			retryStorage := templateStorage
			if restart {
				retryStorage = NewTemplateStorage(newTestClientProvider(t, serverURL))
				t.Cleanup(retryStorage.Destroy)
			}
			updated, err := updateTemplate(namespacedContext("control-plane"), retryStorage, desired)
			if err != nil {
				t.Fatalf("expected the retry to succeed once the reused import finished: %v", err)
			}

			if calls := countMutations(state, mutationsBefore, createTemplateVersionMutation); calls != 1 {
				t.Fatalf("expected exactly one CreateTemplateVersion across both requests, got %d", calls)
			}
			reused := newestVersion(t, state, "reuse-pending", 1)
			if reused.ID != started.ID || updated.Status.ActiveVersionID != started.ID.String() {
				t.Fatalf("expected the retry to activate the first attempt %s, got active %s", started.ID, updated.Status.ActiveVersionID)
			}
		})
	}
}

func TestTemplateStorageUpdateReusesSucceededVersionWithoutNewVersion(t *testing.T) {
	setFastTemplateVersionBuildPolling(t, "5s")
	templateStorage, state, _ := newTemplateReuseHarness(t, nil)
	current, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "reuse-succeeded")

	state.setNextCreatedTemplateVersionStatus(codersdk.ProvisionerJobPending)
	desired := filesUpdate(current, "reuse_succeeded")
	if _, err := updateTemplate(requestTimeoutContext(t, 200*time.Millisecond), templateStorage, desired); !apierrors.IsTimeout(err) {
		t.Fatalf("expected the first update to time out, got %v", err)
	}
	started := newestVersion(t, state, "reuse-succeeded", 1)
	state.setTemplateVersionState(started.ID, codersdk.ProvisionerJobSucceeded, false) // the import finishes later

	mutationsBefore := len(state.mutations())
	updated, err := updateTemplate(namespacedContext("control-plane"), templateStorage, desired)
	if err != nil {
		t.Fatalf("expected the retry to promote the finished attempt: %v", err)
	}
	if calls := countMutations(state, mutationsBefore, createTemplateVersionMutation); calls != 0 {
		t.Fatalf("expected no new template version, got %d CreateTemplateVersion call(s)", calls)
	}
	if uploads := countMutations(state, mutationsBefore, uploadMutation); uploads != 0 {
		t.Fatalf("expected no upload when an attempt is reused, got %d", uploads)
	}
	if updated.Status.ActiveVersionID != started.ID.String() {
		t.Fatalf("expected %s to be active, got %s", started.ID, updated.Status.ActiveVersionID)
	}
	newestVersion(t, state, "reuse-succeeded", 1)
}

// A failed, canceled or archived attempt is never reused: identical source gets the next attempt name.
func TestTemplateStorageUpdateStartsNextAttemptAfterUnusableAttempt(t *testing.T) {
	tests := []struct {
		name     string
		status   codersdk.ProvisionerJobStatus
		archived bool
	}{
		{name: "failed", status: codersdk.ProvisionerJobFailed},
		{name: "canceled", status: codersdk.ProvisionerJobCanceled},
		{name: "canceling", status: codersdk.ProvisionerJobCanceling},
		{name: "archived after success", status: codersdk.ProvisionerJobSucceeded, archived: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setFastTemplateVersionBuildPolling(t, "5s")
			templateStorage, state, _ := newTemplateReuseHarness(t, nil)
			current, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "next-attempt")

			state.setNextCreatedTemplateVersionStatus(codersdk.ProvisionerJobPending)
			desired := filesUpdate(current, "next_attempt")
			if _, err := updateTemplate(requestTimeoutContext(t, 200*time.Millisecond), templateStorage, desired); !apierrors.IsTimeout(err) {
				t.Fatalf("expected the first update to time out, got %v", err)
			}
			first := newestVersion(t, state, "next-attempt", 1)
			state.setTemplateVersionState(first.ID, tc.status, tc.archived)

			updated, err := updateTemplate(namespacedContext("control-plane"), templateStorage, desired)
			if err != nil {
				t.Fatalf("expected the retry to import a new attempt: %v", err)
			}
			second := newestVersion(t, state, "next-attempt", 2)
			if second.Name != first.Name+"-2" {
				t.Fatalf("expected the next attempt to be named %q, got %q", first.Name+"-2", second.Name)
			}
			if updated.Status.ActiveVersionID != second.ID.String() {
				t.Fatalf("expected %s to be active, got %s", second.ID, updated.Status.ActiveVersionID)
			}
		})
	}
}

// Many earlier failed attempts are skipped with O(log n) lookups, not one lookup per attempt.
func TestTemplateStorageUpdateFindsNextAttemptAfterManyFailures(t *testing.T) {
	setFastTemplateVersionBuildPolling(t, "5s")
	var lookups atomic.Int32
	templateStorage, state, _ := newTemplateReuseHarness(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if isTemplateVersionByNameLookup(r) {
			lookups.Add(1)
		}
		return false
	})
	current, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "many-failures")

	state.setNextCreatedTemplateVersionStatus(codersdk.ProvisionerJobFailed)
	desired := filesUpdate(current, "many_failures")
	if _, err := updateTemplate(namespacedContext("control-plane"), templateStorage, desired); !apierrors.IsBadRequest(err) {
		t.Fatalf("expected the first attempt to fail, got %v", err)
	}
	first := newestVersion(t, state, "many-failures", 1)
	template, _ := state.templateByName("acme", "many-failures")
	for attempt := 2; attempt <= 40; attempt++ {
		name, err := templateVersionAttemptName(first.Name, attempt)
		if err != nil {
			t.Fatalf("attempt name: %v", err)
		}
		state.seedTemplateVersion(template.ID, name, first.Job.FileID, codersdk.ProvisionerJobFailed, 0)
	}

	lookups.Store(0)
	updated, err := updateTemplate(namespacedContext("control-plane"), templateStorage, desired)
	if err != nil {
		t.Fatalf("expected attempt 41 to import: %v", err)
	}
	newest := newestVersion(t, state, "many-failures", 41)
	if newest.Name != first.Name+"-41" || updated.Status.ActiveVersionID != newest.ID.String() {
		t.Fatalf("expected %s-41 to be created and active, got %q (active %s)", first.Name, newest.Name, updated.Status.ActiveVersionID)
	}
	// Exponential probe 1,2,4,...,64 (7 lookups) + binary search between 32 and 64 (5 lookups).
	if got := lookups.Load(); got > 12 {
		t.Fatalf("expected at most 12 name lookups for 40 earlier attempts, got %d", got)
	}
}

// Another request creating the same attempt name first makes Coder answer 409; the update must look the
// name up again and wait on that version instead of failing with AlreadyExists or importing again.
func TestTemplateStorageUpdateRelooksUpAfterDuplicateNameConflict(t *testing.T) {
	setFastTemplateVersionBuildPolling(t, "5s")
	var (
		state       *mockCoderServerState
		armed       atomic.Bool
		injectOnce  sync.Once
		injectedID  atomic.Value
		createCalls atomic.Int32
	)
	templateStorage, harnessState, _ := newTemplateReuseHarness(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if !armed.Load() || !isCreateTemplateVersion(r) {
			return false
		}
		createCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read create request: %v", err)
			return false
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var request codersdk.CreateTemplateVersionRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode create request: %v", err)
			return false
		}
		injectOnce.Do(func() {
			// The competing request wins the insert with the same name; its import finishes after 3 polls.
			injectedID.Store(state.seedTemplateVersion(request.TemplateID, request.Name, request.FileID, codersdk.ProvisionerJobPending, 3))
		})
		return false // the mock now answers this create with 409
	})
	state = harnessState
	current, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "name-conflict")
	armed.Store(true)

	updated, err := updateTemplate(namespacedContext("control-plane"), templateStorage, filesUpdate(current, "name_conflict"))
	if err != nil {
		t.Fatalf("expected the update to wait on the competing attempt after the 409: %v", err)
	}
	injected, _ := injectedID.Load().(uuid.UUID)
	if injected == uuid.Nil {
		t.Fatal("assertion failed: the competing attempt was not injected")
	}
	if calls := createCalls.Load(); calls != 1 {
		t.Fatalf("expected one CreateTemplateVersion (answered 409), got %d", calls)
	}
	if updated.Status.ActiveVersionID != injected.String() {
		t.Fatalf("expected the competing attempt %s to become active, got %s", injected, updated.Status.ActiveVersionID)
	}
	newestVersion(t, state, "name-conflict", 1)
}

// Lost responses: Coder applied the request but the reply never arrived.
func TestTemplateStorageUpdateRetryAfterLostResponse(t *testing.T) {
	tests := []struct {
		name string
		lose func(r *http.Request) bool
	}{
		{name: "version created", lose: isCreateTemplateVersion},
		{name: "promotion applied", lose: isActiveVersionPromotion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setFastTemplateVersionBuildPolling(t, "5s")
			var (
				state    *mockCoderServerState
				armed    atomic.Bool
				loseOnce atomic.Bool
			)
			templateStorage, harnessState, _ := newTemplateReuseHarness(t, func(w http.ResponseWriter, r *http.Request) bool {
				if !armed.Load() || !tc.lose(r) || !loseOnce.CompareAndSwap(false, true) {
					return false
				}
				state.handleRequest(t, httptest.NewRecorder(), r)                      // applied in Coder ...
				writeCoderError(w, http.StatusBadGateway, "upstream connection reset") // ... reply lost
				return true
			})
			state = harnessState
			current, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "lost-response")
			mutationsBefore := len(state.mutations())
			armed.Store(true)

			desired := filesUpdate(current, "lost_response")
			if _, err := updateTemplate(namespacedContext("control-plane"), templateStorage, desired); err == nil {
				t.Fatal("expected the first update to fail when the reply is lost")
			}
			// Like kubectl apply, the retry re-reads the template (a lost promotion reply changed its resourceVersion).
			freshObj, err := templateStorage.Get(namespacedContext("control-plane"), current.Name, nil)
			if err != nil {
				t.Fatalf("re-read template: %v", err)
			}
			fresh, ok := freshObj.(*aggregationv1alpha1.CoderTemplate)
			if !ok {
				t.Fatalf("expected *CoderTemplate, got %T", freshObj)
			}
			fresh.Spec.Files = desired.Spec.Files
			updated, err := updateTemplate(namespacedContext("control-plane"), templateStorage, fresh)
			if err != nil {
				t.Fatalf("expected the retry to converge: %v", err)
			}
			if calls := countMutations(state, mutationsBefore, createTemplateVersionMutation); calls != 1 {
				t.Fatalf("expected exactly one CreateTemplateVersion across both requests, got %d", calls)
			}
			only := newestVersion(t, state, "lost-response", 1)
			if updated.Status.ActiveVersionID != only.ID.String() {
				t.Fatalf("expected %s to be active, got %s", only.ID, updated.Status.ActiveVersionID)
			}
		})
	}
}

// Identical source for two templates never shares a version.
func TestTemplateStorageUpdateNeverReusesAnotherTemplatesVersion(t *testing.T) {
	setFastTemplateVersionBuildPolling(t, "5s")
	templateStorage, state, _ := newTemplateReuseHarness(t, nil)
	currentA, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "share-a")
	currentB, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "share-b")

	state.setNextCreatedTemplateVersionStatus(codersdk.ProvisionerJobPending)
	if _, err := updateTemplate(requestTimeoutContext(t, 200*time.Millisecond), templateStorage, filesUpdate(currentA, "shared")); !apierrors.IsTimeout(err) {
		t.Fatalf("expected template A's update to time out, got %v", err)
	}
	versionA := newestVersion(t, state, "share-a", 1)

	updatedB, err := updateTemplate(namespacedContext("control-plane"), templateStorage, filesUpdate(currentB, "shared"))
	if err != nil {
		t.Fatalf("expected template B's update to import its own version: %v", err)
	}
	versionB := newestVersion(t, state, "share-b", 1)
	if versionB.ID == versionA.ID || versionB.Name == versionA.Name {
		t.Fatalf("expected distinct versions and names, got A=%s/%s B=%s/%s", versionA.ID, versionA.Name, versionB.ID, versionB.Name)
	}
	if updatedB.Status.ActiveVersionID != versionB.ID.String() {
		t.Fatalf("expected B's own version to be active, got %s", updatedB.Status.ActiveVersionID)
	}
}

// Scanning stops at the per-request lookup budget and on cancellation, without creating anything.
func TestTemplateStorageUpdateBoundsAttemptLookups(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		setFastTemplateVersionBuildPolling(t, "5s")
		var (
			armed   atomic.Bool
			lookups atomic.Int32
		)
		templateStorage, state, _ := newTemplateReuseHarness(t, func(w http.ResponseWriter, r *http.Request) bool {
			if !armed.Load() {
				return false
			}
			if isTemplateVersionByNameLookup(r) {
				lookups.Add(1)
				return false
			}
			if isCreateTemplateVersion(r) { // every insert loses a race that never becomes visible
				writeJSON(w, http.StatusConflict, codersdk.Response{
					Message:     "A template version with that name already exists for this template.",
					Validations: []codersdk.ValidationError{{Field: "name", Detail: "This value is already in use and should be unique."}},
				})
				return true
			}
			return false
		})
		current, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "lookup-budget")
		versionsBefore := state.templateVersionCount()
		armed.Store(true)

		_, err := updateTemplate(namespacedContext("control-plane"), templateStorage, filesUpdate(current, "lookup_budget"))
		if !apierrors.IsServiceUnavailable(err) || !strings.Contains(err.Error(), "lookups") {
			t.Fatalf("expected 503 after the lookup budget, got %T: %v", err, err)
		}
		assertTopLevelStatusError(t, err)
		if got := lookups.Load(); got != maxTemplateVersionNameLookupsPerRequest {
			t.Fatalf("expected exactly %d lookups, got %d", maxTemplateVersionNameLookupsPerRequest, got)
		}
		if versions := state.templateVersionCount(); versions != versionsBefore {
			t.Fatalf("expected no template version, before=%d after=%d", versionsBefore, versions)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		setFastTemplateVersionBuildPolling(t, "5s")
		ctx, cancel := context.WithCancel(namespacedContext("control-plane"))
		t.Cleanup(cancel)
		var (
			armed   atomic.Bool
			lookups atomic.Int32
		)
		templateStorage, state, _ := newTemplateReuseHarness(t, func(_ http.ResponseWriter, r *http.Request) bool {
			if armed.Load() && isTemplateVersionByNameLookup(r) && lookups.Add(1) == 1 {
				cancel() // the client goes away during the scan
			}
			return false
		})
		current, _ := createTemplateForMetadataUpdate(t, templateStorage, state, "lookup-cancel")
		state.setNextCreatedTemplateVersionStatus(codersdk.ProvisionerJobFailed)
		desired := filesUpdate(current, "lookup_cancel")
		if _, err := updateTemplate(namespacedContext("control-plane"), templateStorage, desired); !apierrors.IsBadRequest(err) {
			t.Fatalf("expected the seed attempt to fail, got %v", err)
		}
		first := newestVersion(t, state, "lookup-cancel", 1)
		template, _ := state.templateByName("acme", "lookup-cancel")
		for attempt := 2; attempt <= 5; attempt++ {
			name, _ := templateVersionAttemptName(first.Name, attempt)
			state.seedTemplateVersion(template.ID, name, first.Job.FileID, codersdk.ProvisionerJobFailed, 0)
		}
		mutationsBefore := len(state.mutations())
		armed.Store(true)

		_, err := updateTemplate(ctx, templateStorage, desired)
		if !apierrors.IsTimeout(err) {
			t.Fatalf("expected a timeout error after cancellation during the scan, got %T: %v", err, err)
		}
		if got := lookups.Load(); got > 2 {
			t.Fatalf("expected the scan to stop right after cancellation, got %d lookups", got)
		}
		if mutations := state.mutations()[mutationsBefore:]; len(mutations) != 0 {
			t.Fatalf("expected no Coder mutation after cancellation, got %v", mutations)
		}
	})
}
