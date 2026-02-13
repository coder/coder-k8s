package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

const (
	watchEventTimeout   = 3 * time.Second
	noWatchEventTimeout = 250 * time.Millisecond
)

func TestTemplateStorageWatch_AddedModifiedDeleted(t *testing.T) {
	t.Parallel()

	server, _ := newMockCoderServer(t)
	defer server.Close()

	templateStorage := NewTemplateStorage(newTestClientProvider(t, server.URL))
	defer templateStorage.Destroy()

	ctx := namespacedContext("control-plane")

	watcher, err := templateStorage.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("start template watch: %v", err)
	}
	defer watcher.Stop()

	templateName := "acme.watch-template"
	createdObj, err := templateStorage.Create(
		ctx,
		&aggregationv1alpha1.CoderTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: templateName},
			Spec: aggregationv1alpha1.CoderTemplateSpec{
				Organization: "acme",
				DisplayName:  "Watch Template",
				Files: map[string]string{
					"main.tf": `resource "null_resource" "watch_template" {}`,
				},
			},
		},
		rest.ValidateAllObjectFunc,
		&metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("create template: %v", err)
	}

	createdTemplate, ok := createdObj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		t.Fatalf("expected *CoderTemplate from create, got %T", createdObj)
	}

	added := receiveWatchEvent(t, watcher, watchEventTimeout)
	if added.Type != watch.Added {
		t.Fatalf("expected Added event, got %s", added.Type)
	}
	addedTemplate := templateFromWatchEvent(t, added)
	if addedTemplate.Name != templateName {
		t.Fatalf("expected Added template name %q, got %q", templateName, addedTemplate.Name)
	}

	desiredTemplate := createdTemplate.DeepCopy()
	desiredTemplate.Spec.DisplayName = "Watch Template Updated"
	_, created, err := templateStorage.Update(
		ctx,
		templateName,
		testUpdatedObjectInfo{obj: desiredTemplate},
		nil,
		rest.ValidateAllObjectUpdateFunc,
		false,
		&metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("update template: %v", err)
	}
	if created {
		t.Fatal("expected template update created=false")
	}

	modified := receiveWatchEvent(t, watcher, watchEventTimeout)
	if modified.Type != watch.Modified {
		t.Fatalf("expected Modified event, got %s", modified.Type)
	}
	modifiedTemplate := templateFromWatchEvent(t, modified)
	if modifiedTemplate.Name != templateName {
		t.Fatalf("expected Modified template name %q, got %q", templateName, modifiedTemplate.Name)
	}

	_, deleted, err := templateStorage.Delete(
		ctx,
		templateName,
		rest.ValidateAllObjectFunc,
		&metav1.DeleteOptions{},
	)
	if err != nil {
		t.Fatalf("delete template: %v", err)
	}
	if !deleted {
		t.Fatal("expected template delete to report deleted=true")
	}

	deletedEvent := receiveWatchEvent(t, watcher, watchEventTimeout)
	if deletedEvent.Type != watch.Deleted {
		t.Fatalf("expected Deleted event, got %s", deletedEvent.Type)
	}
	deletedTemplate := templateFromWatchEvent(t, deletedEvent)
	if deletedTemplate.Name != templateName {
		t.Fatalf("expected Deleted template name %q, got %q", templateName, deletedTemplate.Name)
	}
}

func TestWorkspaceStorageWatch_AddedModified(t *testing.T) {
	t.Parallel()

	server, _ := newMockCoderServer(t)
	defer server.Close()

	workspaceStorage := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	defer workspaceStorage.Destroy()

	ctx := namespacedContext("control-plane")

	watcher, err := workspaceStorage.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("start workspace watch: %v", err)
	}
	defer watcher.Stop()

	workspaceName := "acme.alice.watch-workspace"
	createdObj, err := workspaceStorage.Create(
		ctx,
		&aggregationv1alpha1.CoderWorkspace{
			ObjectMeta: metav1.ObjectMeta{Name: workspaceName},
			Spec: aggregationv1alpha1.CoderWorkspaceSpec{
				Organization: "acme",
				TemplateName: "starter-template",
				Running:      false,
			},
		},
		rest.ValidateAllObjectFunc,
		&metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	createdWorkspace, ok := createdObj.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		t.Fatalf("expected *CoderWorkspace from create, got %T", createdObj)
	}

	added := receiveWatchEvent(t, watcher, watchEventTimeout)
	if added.Type != watch.Added {
		t.Fatalf("expected Added event, got %s", added.Type)
	}
	addedWorkspace := workspaceFromWatchEvent(t, added)
	if addedWorkspace.Name != workspaceName {
		t.Fatalf("expected Added workspace name %q, got %q", workspaceName, addedWorkspace.Name)
	}

	desiredWorkspace := createdWorkspace.DeepCopy()
	desiredWorkspace.Spec.Running = true
	_, created, err := workspaceStorage.Update(
		ctx,
		workspaceName,
		testUpdatedObjectInfo{obj: desiredWorkspace},
		nil,
		rest.ValidateAllObjectUpdateFunc,
		false,
		&metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("update workspace: %v", err)
	}
	if created {
		t.Fatal("expected workspace update created=false")
	}

	modified := receiveWatchEvent(t, watcher, watchEventTimeout)
	if modified.Type != watch.Modified {
		t.Fatalf("expected Modified event, got %s", modified.Type)
	}
	modifiedWorkspace := workspaceFromWatchEvent(t, modified)
	if modifiedWorkspace.Name != workspaceName {
		t.Fatalf("expected Modified workspace name %q, got %q", workspaceName, modifiedWorkspace.Name)
	}
	if !modifiedWorkspace.Spec.Running {
		t.Fatal("expected Modified workspace event with running=true")
	}
}

func TestWatchRespectsFieldSelectorMetadataName(t *testing.T) {
	t.Parallel()

	server, _ := newMockCoderServer(t)
	defer server.Close()

	templateStorage := NewTemplateStorage(newTestClientProvider(t, server.URL))
	defer templateStorage.Destroy()

	ctx := namespacedContext("control-plane")
	targetName := "acme.target-template"

	watcher, err := templateStorage.Watch(ctx, &metainternalversion.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", targetName),
	})
	if err != nil {
		t.Fatalf("start template watch with field selector: %v", err)
	}
	defer watcher.Stop()

	_, err = templateStorage.Create(
		ctx,
		&aggregationv1alpha1.CoderTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "acme.non-target-template"},
			Spec: aggregationv1alpha1.CoderTemplateSpec{
				Organization: "acme",
				VersionID:    uuid.NewString(),
				DisplayName:  "Non Target",
			},
		},
		rest.ValidateAllObjectFunc,
		&metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("create non-matching template: %v", err)
	}
	assertNoWatchEvent(t, watcher, noWatchEventTimeout)

	_, err = templateStorage.Create(
		ctx,
		&aggregationv1alpha1.CoderTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: targetName},
			Spec: aggregationv1alpha1.CoderTemplateSpec{
				Organization: "acme",
				VersionID:    uuid.NewString(),
				DisplayName:  "Target",
			},
		},
		rest.ValidateAllObjectFunc,
		&metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("create matching template: %v", err)
	}

	event := receiveWatchEvent(t, watcher, watchEventTimeout)
	if event.Type != watch.Added {
		t.Fatalf("expected Added event for matching template, got %s", event.Type)
	}
	gotTemplate := templateFromWatchEvent(t, event)
	if gotTemplate.Name != targetName {
		t.Fatalf("expected matching template name %q, got %q", targetName, gotTemplate.Name)
	}
}

func TestWatchStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	server, _ := newMockCoderServer(t)
	defer server.Close()

	templateStorage := NewTemplateStorage(newTestClientProvider(t, server.URL))
	defer templateStorage.Destroy()

	ctx, cancel := context.WithCancel(namespacedContext("control-plane"))
	watcher, err := templateStorage.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("start template watch: %v", err)
	}
	defer watcher.Stop()

	cancel()
	assertWatchClosed(t, watcher, watchEventTimeout)
}

func TestWorkspaceWatchRejectsUnsupportedWatchListOptions(t *testing.T) {
	t.Parallel()

	server, _ := newMockCoderServer(t)
	defer server.Close()

	workspaceStorage := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	defer workspaceStorage.Destroy()

	ctx := namespacedContext("control-plane")

	t.Run("sendInitialEvents", func(t *testing.T) {
		sendInitialEvents := true
		watcher, err := workspaceStorage.Watch(ctx, &metainternalversion.ListOptions{SendInitialEvents: &sendInitialEvents})
		assertBadRequestWatchOptionsError(t, watcher, err, "sendInitialEvents")
	})

	t.Run("resourceVersion", func(t *testing.T) {
		watcher, err := workspaceStorage.Watch(ctx, &metainternalversion.ListOptions{ResourceVersion: "123"})
		if err != nil {
			t.Fatalf("expected resourceVersion watch to be accepted, got %v", err)
		}
		if watcher == nil {
			t.Fatal("assertion failed: watcher must not be nil")
		}
		watcher.Stop()
	})

	t.Run("legacyResourceVersionZero", func(t *testing.T) {
		watcher, err := workspaceStorage.Watch(ctx, &metainternalversion.ListOptions{ResourceVersion: "0"})
		if err != nil {
			t.Fatalf("expected resourceVersion=0 watch to be accepted, got %v", err)
		}
		if watcher == nil {
			t.Fatal("assertion failed: watcher must not be nil")
		}
		watcher.Stop()
	})

	t.Run("resourceVersionMatch", func(t *testing.T) {
		watcher, err := workspaceStorage.Watch(ctx, &metainternalversion.ListOptions{
			ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		})
		assertBadRequestWatchOptionsError(t, watcher, err, "resourceVersionMatch")
	})
}

func TestTemplateWatchRejectsUnsupportedWatchListOptions(t *testing.T) {
	t.Parallel()

	server, _ := newMockCoderServer(t)
	defer server.Close()

	templateStorage := NewTemplateStorage(newTestClientProvider(t, server.URL))
	defer templateStorage.Destroy()

	ctx := namespacedContext("control-plane")

	t.Run("sendInitialEvents", func(t *testing.T) {
		sendInitialEvents := true
		watcher, err := templateStorage.Watch(ctx, &metainternalversion.ListOptions{SendInitialEvents: &sendInitialEvents})
		assertBadRequestWatchOptionsError(t, watcher, err, "sendInitialEvents")
	})

	t.Run("resourceVersion", func(t *testing.T) {
		watcher, err := templateStorage.Watch(ctx, &metainternalversion.ListOptions{ResourceVersion: "123"})
		if err != nil {
			t.Fatalf("expected resourceVersion watch to be accepted, got %v", err)
		}
		if watcher == nil {
			t.Fatal("assertion failed: watcher must not be nil")
		}
		watcher.Stop()
	})

	t.Run("legacyResourceVersionZero", func(t *testing.T) {
		watcher, err := templateStorage.Watch(ctx, &metainternalversion.ListOptions{ResourceVersion: "0"})
		if err != nil {
			t.Fatalf("expected resourceVersion=0 watch to be accepted, got %v", err)
		}
		if watcher == nil {
			t.Fatal("assertion failed: watcher must not be nil")
		}
		watcher.Stop()
	})

	t.Run("resourceVersionMatch", func(t *testing.T) {
		watcher, err := templateStorage.Watch(ctx, &metainternalversion.ListOptions{
			ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		})
		assertBadRequestWatchOptionsError(t, watcher, err, "resourceVersionMatch")
	})
}

func TestWatchRejectsDefaultedLegacyWatchListOptions(t *testing.T) {
	t.Parallel()

	server, _ := newMockCoderServer(t)
	defer server.Close()

	workspaceStorage := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	defer workspaceStorage.Destroy()
	templateStorage := NewTemplateStorage(newTestClientProvider(t, server.URL))
	defer templateStorage.Destroy()

	ctx := namespacedContext("control-plane")
	sendInitialEvents := true

	legacyOptionsWithEmptyRV := &metainternalversion.ListOptions{
		Watch:                true,
		ResourceVersion:      "",
		SendInitialEvents:    &sendInitialEvents,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
	}
	workspaceWatcher, err := workspaceStorage.Watch(ctx, legacyOptionsWithEmptyRV)
	assertBadRequestWatchOptionsError(t, workspaceWatcher, err, "sendInitialEvents")
	templateWatcher, err := templateStorage.Watch(ctx, legacyOptionsWithEmptyRV)
	assertBadRequestWatchOptionsError(t, templateWatcher, err, "sendInitialEvents")

	legacyOptionsWithZeroRV := &metainternalversion.ListOptions{
		Watch:                true,
		ResourceVersion:      "0",
		SendInitialEvents:    &sendInitialEvents,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
	}
	workspaceWatcher, err = workspaceStorage.Watch(ctx, legacyOptionsWithZeroRV)
	assertBadRequestWatchOptionsError(t, workspaceWatcher, err, "sendInitialEvents")
	templateWatcher, err = templateStorage.Watch(ctx, legacyOptionsWithZeroRV)
	assertBadRequestWatchOptionsError(t, templateWatcher, err, "sendInitialEvents")
}

func TestValidateUnsupportedWatchListOptions(t *testing.T) {
	t.Parallel()

	if err := validateUnsupportedWatchListOptions(nil); err != nil {
		t.Fatalf("expected nil list options to be accepted, got %v", err)
	}

	if err := validateUnsupportedWatchListOptions(&metainternalversion.ListOptions{}); err != nil {
		t.Fatalf("expected empty list options to be accepted, got %v", err)
	}

	sendInitialEventsDisabled := false
	if err := validateUnsupportedWatchListOptions(&metainternalversion.ListOptions{SendInitialEvents: &sendInitialEventsDisabled}); err != nil {
		t.Fatalf("expected sendInitialEvents=false to be accepted, got %v", err)
	}

	sendInitialEvents := true
	legacyOptionsWithEmptyRV := &metainternalversion.ListOptions{
		Watch:                true,
		ResourceVersion:      "",
		SendInitialEvents:    &sendInitialEvents,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
	}
	err := validateUnsupportedWatchListOptions(legacyOptionsWithEmptyRV)
	if err == nil {
		t.Fatal("expected defaulted legacy watch-list options with empty RV to be rejected")
	}
	if !strings.Contains(err.Error(), "sendInitialEvents") {
		t.Fatalf("expected defaulted legacy watch-list rejection to reference sendInitialEvents, got %v", err)
	}

	legacyOptionsWithZeroRV := &metainternalversion.ListOptions{
		Watch:                true,
		ResourceVersion:      "0",
		SendInitialEvents:    &sendInitialEvents,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
	}
	err = validateUnsupportedWatchListOptions(legacyOptionsWithZeroRV)
	if err == nil {
		t.Fatal("expected defaulted legacy watch-list options with RV=0 to be rejected")
	}
	if !strings.Contains(err.Error(), "sendInitialEvents") {
		t.Fatalf("expected defaulted legacy watch-list RV=0 rejection to reference sendInitialEvents, got %v", err)
	}

	err = validateUnsupportedWatchListOptions(&metainternalversion.ListOptions{SendInitialEvents: &sendInitialEvents})
	if err == nil {
		t.Fatal("expected sendInitialEvents=true to be rejected")
	}
	if !strings.Contains(err.Error(), "sendInitialEvents") {
		t.Fatalf("expected sendInitialEvents error, got %v", err)
	}

	err = validateUnsupportedWatchListOptions(&metainternalversion.ListOptions{ResourceVersion: "123"})
	if err != nil {
		t.Fatalf("expected resourceVersion to be accepted, got %v", err)
	}

	err = validateUnsupportedWatchListOptions(&metainternalversion.ListOptions{
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
	})
	if err == nil {
		t.Fatal("expected resourceVersionMatch without matching legacy defaults to be rejected")
	}
	if !strings.Contains(err.Error(), "resourceVersionMatch") {
		t.Fatalf("expected resourceVersionMatch error, got %v", err)
	}

	err = validateUnsupportedWatchListOptions(&metainternalversion.ListOptions{
		Watch:                true,
		ResourceVersion:      "12345",
		SendInitialEvents:    &sendInitialEvents,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
	})
	if err == nil {
		t.Fatal("expected non-legacy watch-list options to be rejected")
	}
	if !strings.Contains(err.Error(), "sendInitialEvents") {
		t.Fatalf("expected non-legacy watch-list rejection to reference sendInitialEvents, got %v", err)
	}
}

func TestValidateFieldSelector(t *testing.T) {
	t.Parallel()

	if err := validateFieldSelector(nil); err != nil {
		t.Fatalf("expected nil field selector to be accepted, got %v", err)
	}

	if err := validateFieldSelector(fields.Everything()); err != nil {
		t.Fatalf("expected empty field selector to be accepted, got %v", err)
	}

	if err := validateFieldSelector(fields.OneTermEqualSelector("metadata.name", "acme.template")); err != nil {
		t.Fatalf("expected metadata.name selector to be accepted, got %v", err)
	}

	if err := validateFieldSelector(fields.OneTermEqualSelector("metadata.namespace", "control-plane")); err != nil {
		t.Fatalf("expected metadata.namespace selector to be accepted, got %v", err)
	}

	err := validateFieldSelector(fields.OneTermEqualSelector("spec.foo", "bar"))
	if err == nil {
		t.Fatal("expected unsupported field selector to return error")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected unsupported field selector error, got %v", err)
	}
}

func TestFilterForListOptions(t *testing.T) {
	t.Parallel()

	t.Run("nil opts returns nil filter", func(t *testing.T) {
		filter, err := filterForListOptions("", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if filter != nil {
			t.Fatal("expected nil filter when namespace and opts are empty")
		}
	})

	t.Run("namespace filtering", func(t *testing.T) {
		filter, err := filterForListOptions("control-plane", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if filter == nil {
			t.Fatal("expected namespace filter to be non-nil")
		}

		if _, ok := filter(templateWatchEvent("acme.match", "control-plane", nil)); !ok {
			t.Fatal("expected namespace-matching event to pass")
		}
		if _, ok := filter(templateWatchEvent("acme.match", "other-namespace", nil)); ok {
			t.Fatal("expected namespace-mismatched event to be filtered out")
		}
	})

	t.Run("label selector filtering", func(t *testing.T) {
		filter, err := filterForListOptions("", &metainternalversion.ListOptions{
			LabelSelector: labels.SelectorFromSet(labels.Set{"env": "prod"}),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if filter == nil {
			t.Fatal("expected label selector filter to be non-nil")
		}

		if _, ok := filter(templateWatchEvent("acme.match", "", map[string]string{"env": "prod"})); !ok {
			t.Fatal("expected label-matching event to pass")
		}
		if _, ok := filter(templateWatchEvent("acme.match", "", map[string]string{"env": "dev"})); ok {
			t.Fatal("expected label-mismatched event to be filtered out")
		}
	})

	t.Run("field selector filtering", func(t *testing.T) {
		filter, err := filterForListOptions("", &metainternalversion.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("metadata.name", "acme.match"),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if filter == nil {
			t.Fatal("expected field selector filter to be non-nil")
		}

		if _, ok := filter(templateWatchEvent("acme.match", "", nil)); !ok {
			t.Fatal("expected field-matching event to pass")
		}
		if _, ok := filter(templateWatchEvent("acme.other", "", nil)); ok {
			t.Fatal("expected field-mismatched event to be filtered out")
		}
	})

	t.Run("combined namespace label and field filtering", func(t *testing.T) {
		filter, err := filterForListOptions("control-plane", &metainternalversion.ListOptions{
			LabelSelector: labels.SelectorFromSet(labels.Set{"team": "platform"}),
			FieldSelector: fields.OneTermEqualSelector("metadata.name", "acme.match"),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if filter == nil {
			t.Fatal("expected combined filter to be non-nil")
		}

		matching := templateWatchEvent("acme.match", "control-plane", map[string]string{"team": "platform"})
		if _, ok := filter(matching); !ok {
			t.Fatal("expected event matching all filters to pass")
		}

		namespaceMismatch := templateWatchEvent("acme.match", "other", map[string]string{"team": "platform"})
		if _, ok := filter(namespaceMismatch); ok {
			t.Fatal("expected namespace mismatch to be filtered out")
		}

		labelMismatch := templateWatchEvent("acme.match", "control-plane", map[string]string{"team": "ops"})
		if _, ok := filter(labelMismatch); ok {
			t.Fatal("expected label mismatch to be filtered out")
		}

		fieldMismatch := templateWatchEvent("acme.other", "control-plane", map[string]string{"team": "platform"})
		if _, ok := filter(fieldMismatch); ok {
			t.Fatal("expected field mismatch to be filtered out")
		}
	})
}

func receiveWatchEvent(t *testing.T, watcher watch.Interface, timeout time.Duration) watch.Event {
	t.Helper()

	select {
	case evt, ok := <-watcher.ResultChan():
		if !ok {
			t.Fatal("watcher channel closed unexpectedly")
		}
		return evt
	case <-time.After(timeout):
		t.Fatal("timed out waiting for watch event")
		return watch.Event{} // unreachable
	}
}

func assertNoWatchEvent(t *testing.T, watcher watch.Interface, timeout time.Duration) {
	t.Helper()

	select {
	case evt, ok := <-watcher.ResultChan():
		if ok {
			t.Fatalf("expected no watch event, got %s %T", evt.Type, evt.Object)
		}
	case <-time.After(timeout):
		// Good: no event received.
	}
}

func assertWatchClosed(t *testing.T, watcher watch.Interface, timeout time.Duration) {
	t.Helper()

	select {
	case _, ok := <-watcher.ResultChan():
		if ok {
			t.Fatal("expected watcher channel to be closed")
		}
	case <-time.After(timeout):
		t.Fatal("timed out waiting for watcher channel to close")
	}
}

func assertBadRequestWatchOptionsError(t *testing.T, watcher watch.Interface, err error, wantErrSubstring string) {
	t.Helper()

	if err == nil {
		if watcher != nil {
			watcher.Stop()
		}
		t.Fatalf("expected watch options error containing %q", wantErrSubstring)
	}
	if watcher != nil {
		watcher.Stop()
		t.Fatalf("expected watcher to be nil when watch options are rejected, got %T", watcher)
	}
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected bad request error, got %v", err)
	}
	if !strings.Contains(err.Error(), wantErrSubstring) {
		t.Fatalf("expected error containing %q, got %v", wantErrSubstring, err)
	}
}

func templateFromWatchEvent(t *testing.T, evt watch.Event) *aggregationv1alpha1.CoderTemplate {
	t.Helper()

	template, ok := evt.Object.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		t.Fatalf("expected watch object *CoderTemplate, got %T", evt.Object)
	}
	if template == nil {
		t.Fatal("assertion failed: template watch object must not be nil")
	}

	return template
}

func workspaceFromWatchEvent(t *testing.T, evt watch.Event) *aggregationv1alpha1.CoderWorkspace {
	t.Helper()

	workspace, ok := evt.Object.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		t.Fatalf("expected watch object *CoderWorkspace, got %T", evt.Object)
	}
	if workspace == nil {
		t.Fatal("assertion failed: workspace watch object must not be nil")
	}

	return workspace
}

func templateWatchEvent(name, namespace string, objectLabels map[string]string) watch.Event {
	return watch.Event{
		Type: watch.Added,
		Object: &aggregationv1alpha1.CoderTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    objectLabels,
			},
		},
	}
}
