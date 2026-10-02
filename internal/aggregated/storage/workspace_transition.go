package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/util/dryrun"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder-k8s/internal/aggregated/convert"
)

const (
	// transitionBudget bounds every Coder call of one start or stop request: the lookup, the
	// build POST, the re-read after a Coder 409, and the confirming re-read after an uncertain
	// POST. Each call also keeps the client's own timeout. The generic create handler ends the
	// request at 34 s.
	transitionBudget = 25 * time.Second
	// transitionConfirmTimeout is the part of transitionBudget that is kept for the confirming
	// re-read after an uncertain POST. The calls before it get the rest.
	transitionConfirmTimeout = 5 * time.Second
)

// Outcomes reported in CoderWorkspaceTransitionStatus.Outcome.
const (
	transitionOutcomeQueued     = "Queued"
	transitionOutcomeInProgress = "InProgress"
	transitionOutcomeUnchanged  = "Unchanged"
	transitionOutcomeWouldQueue = "WouldQueue"
)

// WorkspaceTransitionStorage serves the coderworkspaces/start or coderworkspaces/stop
// subresource. It decides from the latest build whether a new build is needed, so a repeated
// request converges on the requested state instead of queueing more builds.
type WorkspaceTransitionStorage struct {
	workspaces     *WorkspaceStorage
	transition     codersdk.WorkspaceTransition
	budget         time.Duration
	confirmTimeout time.Duration
}

var (
	_ rest.Storage      = (*WorkspaceTransitionStorage)(nil)
	_ rest.NamedCreater = (*WorkspaceTransitionStorage)(nil) //nolint:misspell // Kubernetes rest interface name is Creater.
)

// NewWorkspaceTransitionStorage returns storage for the start or stop transition.
func NewWorkspaceTransitionStorage(workspaces *WorkspaceStorage, transition codersdk.WorkspaceTransition) *WorkspaceTransitionStorage {
	if workspaces == nil {
		panic("assertion failed: workspace storage must not be nil")
	}
	if transition != codersdk.WorkspaceTransitionStart && transition != codersdk.WorkspaceTransitionStop {
		panic(fmt.Sprintf("assertion failed: unsupported workspace transition %q", transition))
	}
	return &WorkspaceTransitionStorage{
		workspaces:     workspaces,
		transition:     transition,
		budget:         transitionBudget,
		confirmTimeout: transitionConfirmTimeout,
	}
}

// New implements rest.Storage.
func (s *WorkspaceTransitionStorage) New() runtime.Object {
	return &aggregationv1alpha1.CoderWorkspaceTransition{}
}

// Destroy implements rest.Storage.
func (s *WorkspaceTransitionStorage) Destroy() {}

// Create starts or stops the workspace name. It always answers with a fresh object that holds
// only metadata and the outcome.
func (s *WorkspaceTransitionStorage) Create(
	ctx context.Context,
	name string,
	obj runtime.Object,
	createValidation rest.ValidateObjectFunc,
	options *metav1.CreateOptions,
) (runtime.Object, error) {
	if s == nil || ctx == nil || obj == nil {
		return nil, fmt.Errorf("assertion failed: transition storage, context, and object must not be nil")
	}
	if name == "" {
		return nil, fmt.Errorf("assertion failed: workspace name must not be empty")
	}
	body, ok := obj.(*aggregationv1alpha1.CoderWorkspaceTransition)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected *CoderWorkspaceTransition, got %T", obj))
	}
	if body.Name != "" && body.Name != name {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("metadata.name %q in the request body must match the workspace name %q in the URL", body.Name, name))
	}
	namespace, err := requiredNamespaceFromRequestContext(ctx)
	if err != nil {
		return nil, err
	}
	// Admission runs for dry-run requests too.
	if createValidation != nil {
		if err := createValidation(ctx, obj); err != nil {
			return nil, err
		}
	}
	isDryRun := options != nil && dryrun.IsDryRun(options.DryRun)
	if s.confirmTimeout <= 0 || s.budget <= s.confirmTimeout {
		return nil, fmt.Errorf("assertion failed: transition budget %s must exceed the confirm timeout %s", s.budget, s.confirmTimeout)
	}

	// One budget covers every Coder call. The calls before the confirming re-read end early
	// enough to leave confirmTimeout of it.
	budgetCtx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()
	callCtx, cancelCalls := context.WithTimeout(budgetCtx, s.budget-s.confirmTimeout)
	defer cancelCalls()
	sdk, workspace, err := s.workspaces.resolveWorkspace(callCtx, namespace, name)
	if err != nil {
		return nil, err
	}
	outcome, err := decideTransition(s.transition, name, workspace.LatestBuild)
	if err != nil {
		return nil, err
	}
	if outcome != transitionOutcomeQueued {
		return s.result(namespace, name, outcome, isDryRun, &workspace.LatestBuild), nil
	}
	if isDryRun {
		return s.result(namespace, name, transitionOutcomeWouldQueue, true, nil), nil
	}

	req := codersdk.CreateWorkspaceBuildRequest{Transition: s.transition}
	if s.transition == codersdk.WorkspaceTransitionStart {
		if req, err = startBuildRequest(workspace); err != nil {
			return nil, err
		}
	}
	newBuild, err := sdk.CreateWorkspaceBuild(callCtx, workspace.ID, req)
	if err == nil {
		return s.queued(namespace, name, workspace, newBuild)
	}

	var coderErr *codersdk.Error
	isCoderErr := errors.As(err, &coderErr)
	switch {
	case isCoderErr && coderErr.StatusCode() == http.StatusConflict:
		return s.afterConflict(callCtx, sdk, namespace, name, workspace.ID, coderErr)
	case !isCoderErr || isUncertainStatus(coderErr.StatusCode()):
		// A timeout, a transport error, or a gateway error leaves it open whether Coder queued
		// the build: a proxy in front of Coder can fail after Coder accepted it. Never
		// POST again: one confirming re-read either finds the new build or reports the doubt.
		// The re-read gets the rest of the budget, at least confirmTimeout.
		current, rereadErr := sdk.Workspace(budgetCtx, workspace.ID)
		if rereadErr == nil && current.LatestBuild.ID != workspace.LatestBuild.ID && current.LatestBuild.Transition == s.transition {
			return s.queued(namespace, name, current, current.LatestBuild)
		}
		return nil, apierrors.NewTimeoutError(fmt.Sprintf(
			"the result is uncertain: the Coder API did not confirm the %s build in time; re-read the workspace's latest build before you retry",
			s.transition), 0)
	default:
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), name)
	}
}

// isUncertainStatus reports whether a Coder or proxy status leaves it open whether the build
// was queued.
func isUncertainStatus(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// afterConflict handles a Coder 409 on the build POST: another build became active after the
// lookup. One re-read decides whether that build already does what was requested.
func (s *WorkspaceTransitionStorage) afterConflict(
	ctx context.Context,
	sdk *codersdk.Client,
	namespace, name string,
	workspaceID uuid.UUID,
	coderErr *codersdk.Error,
) (runtime.Object, error) {
	current, err := sdk.Workspace(ctx, workspaceID)
	if err != nil {
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), name)
	}
	outcome, err := decideTransition(s.transition, name, current.LatestBuild)
	if err != nil && !apierrors.IsConflict(err) {
		return nil, err
	}
	if err == nil && outcome != transitionOutcomeQueued {
		return s.result(namespace, name, outcome, false, &current.LatestBuild), nil
	}
	return nil, apierrors.NewConflict(aggregationv1alpha1.Resource("coderworkspaces"), name,
		fmt.Errorf("%s; retry after the active build ends", coderErr.Message))
}

// queued answers Queued for a build that this request queued. Like an update of spec.running, it
// sends a Modified watch event for the workspace with that build as the latest build.
func (s *WorkspaceTransitionStorage) queued(namespace, name string, workspace codersdk.Workspace, build codersdk.WorkspaceBuild) (runtime.Object, error) {
	workspace.LatestBuild = build
	obj := convert.WorkspaceToK8s(namespace, workspace)
	if obj == nil {
		return nil, fmt.Errorf("assertion failed: converted workspace must not be nil")
	}
	s.workspaces.enqueueWatchEvent(watch.Modified, obj)
	return s.result(namespace, name, transitionOutcomeQueued, false, &build), nil
}

func (s *WorkspaceTransitionStorage) result(namespace, name, outcome string, isDryRun bool, build *codersdk.WorkspaceBuild) *aggregationv1alpha1.CoderWorkspaceTransition {
	result := &aggregationv1alpha1.CoderWorkspaceTransition{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: aggregationv1alpha1.CoderWorkspaceTransitionStatus{
			Transition: string(s.transition),
			Outcome:    outcome,
			DryRun:     isDryRun,
		},
	}
	if build != nil {
		result.Status.BuildID = build.ID.String()
		result.Status.BuildNumber = build.BuildNumber
		result.Status.JobStatus = string(build.Job.Status)
	}
	return result
}

// decideTransition picks the outcome of a start or stop request from the latest build. It
// returns Queued when a new build is needed, InProgress or Unchanged when the latest build
// already does what was requested, and an error otherwise.
func decideTransition(want codersdk.WorkspaceTransition, name string, latest codersdk.WorkspaceBuild) (string, error) {
	switch latest.Transition {
	case codersdk.WorkspaceTransitionStart, codersdk.WorkspaceTransitionStop, codersdk.WorkspaceTransitionDelete:
	default:
		return "", fmt.Errorf("assertion failed: latest build of workspace %q has unknown transition %q", name, latest.Transition)
	}
	conflict := func(format string) error {
		return apierrors.NewConflict(aggregationv1alpha1.Resource("coderworkspaces"), name,
			fmt.Errorf(format+"; retry after it ends", latest.Transition, latest.BuildNumber, latest.Job.Status))
	}
	switch latest.Job.Status {
	case codersdk.ProvisionerJobPending, codersdk.ProvisionerJobRunning:
		if latest.Transition == want {
			return transitionOutcomeInProgress, nil
		}
		return "", conflict("a %s build is active (build #%d, job %s)")
	case codersdk.ProvisionerJobCanceling:
		return "", conflict("a %s build is being canceled (build #%d, job %s)")
	case codersdk.ProvisionerJobSucceeded:
		switch latest.Transition {
		case want:
			return transitionOutcomeUnchanged, nil
		case codersdk.WorkspaceTransitionDelete:
			// Coder does not return deleted workspaces, so this only happens in a race.
			return "", apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
		}
		return transitionOutcomeQueued, nil
	case codersdk.ProvisionerJobFailed, codersdk.ProvisionerJobCanceled:
		return transitionOutcomeQueued, nil
	default:
		return "", fmt.Errorf("assertion failed: latest build of workspace %q has unknown job status %q", name, latest.Job.Status)
	}
}

// startBuildRequest builds a start request. Like the coder CLI, it sends the template's active
// version when the template requires it or the workspace always updates. Otherwise Coder reuses
// the previous build's version.
func startBuildRequest(workspace codersdk.Workspace) (codersdk.CreateWorkspaceBuildRequest, error) {
	req := codersdk.CreateWorkspaceBuildRequest{Transition: codersdk.WorkspaceTransitionStart}
	if workspace.TemplateRequireActiveVersion || workspace.AutomaticUpdates == codersdk.AutomaticUpdatesAlways {
		if workspace.TemplateActiveVersionID == uuid.Nil {
			return req, fmt.Errorf("assertion failed: workspace %q must carry the template's active version ID", workspace.Name)
		}
		req.TemplateVersionID = workspace.TemplateActiveVersionID
	}
	return req, nil
}
