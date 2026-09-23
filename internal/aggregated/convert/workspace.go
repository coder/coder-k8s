package convert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// WorkspaceToK8s converts a codersdk.Workspace to an aggregated API CoderWorkspace.
//
// metadata.resourceVersion is an opaque fingerprint of the returned representation: the full
// SHA-256 of the converted object serialized with resourceVersion unset. Every emitted
// metadata/spec/status field takes part, including lastUsedAt and autoShutdown, so a change that
// this API exposes changes the token even when Coder's Workspace.UpdatedAt stays the same, and a
// backend-only change that leaves the projection identical keeps the token. It is not monotonic
// and not a history cursor: an identical projection yields the same token again.
func WorkspaceToK8s(namespace string, w codersdk.Workspace) *aggregationv1alpha1.CoderWorkspace {
	if namespace == "" {
		panic("assertion failed: namespace must not be empty")
	}

	var autoShutdown *metav1.Time
	if w.LatestBuild.Deadline.Valid && !w.LatestBuild.Deadline.Time.IsZero() {
		autoShutdownTime := metav1.NewTime(w.LatestBuild.Deadline.Time)
		autoShutdown = &autoShutdownTime
	}
	lastUsedAt := metav1.NewTime(w.LastUsedAt)

	obj := &aggregationv1alpha1.CoderWorkspace{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CoderWorkspace",
			APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              coder.BuildWorkspaceName(w.OrganizationName, w.OwnerName, w.Name),
			Namespace:         namespace,
			UID:               types.UID(w.ID.String()),
			CreationTimestamp: metav1.NewTime(w.CreatedAt),
		},
		Spec: aggregationv1alpha1.CoderWorkspaceSpec{
			Organization:      w.OrganizationName,
			TemplateName:      w.TemplateName,
			TemplateVersionID: w.LatestBuild.TemplateVersionID.String(),
			Running:           workspaceRunning(w),
			TTLMillis:         w.TTLMillis,
			AutostartSchedule: w.AutostartSchedule,
		},
		Status: aggregationv1alpha1.CoderWorkspaceStatus{
			ID:                w.ID.String(),
			OwnerName:         w.OwnerName,
			OrganizationName:  w.OrganizationName,
			TemplateName:      w.TemplateName,
			LatestBuildID:     w.LatestBuild.ID.String(),
			LatestBuildStatus: string(w.LatestBuild.Status),
			AutoShutdown:      autoShutdown,
			LastUsedAt:        &lastUsedAt,
		},
	}

	// Serialization of the generated API type cannot fail for well-formed input, so an error is an
	// impossible state.
	serialized, err := json.Marshal(obj)
	if err != nil {
		panic(fmt.Sprintf("assertion failed: serialize workspace representation: %v", err))
	}
	sum := sha256.Sum256(serialized)
	obj.ResourceVersion = hex.EncodeToString(sum[:])

	return obj
}

func workspaceRunning(workspace codersdk.Workspace) bool {
	if workspace.LatestBuild.Transition != codersdk.WorkspaceTransitionStart {
		return false
	}

	switch workspace.LatestBuild.Status {
	case codersdk.WorkspaceStatusPending, codersdk.WorkspaceStatusStarting, codersdk.WorkspaceStatusRunning:
		return true
	default:
		return false
	}
}

// WorkspaceCreateRequestFromK8s builds a codersdk.CreateWorkspaceRequest.
func WorkspaceCreateRequestFromK8s(
	obj *aggregationv1alpha1.CoderWorkspace,
	workspaceName string,
	templateID uuid.UUID,
) (codersdk.CreateWorkspaceRequest, error) {
	if obj == nil {
		panic("assertion failed: workspace object must not be nil")
	}
	if workspaceName == "" {
		panic("assertion failed: workspace name must not be empty")
	}
	if templateID == uuid.Nil {
		panic("assertion failed: template ID must not be nil")
	}

	request := codersdk.CreateWorkspaceRequest{
		Name:              workspaceName,
		TTLMillis:         obj.Spec.TTLMillis,
		AutostartSchedule: obj.Spec.AutostartSchedule,
	}

	if obj.Spec.TemplateVersionID == "" {
		request.TemplateID = templateID
		return request, nil
	}

	templateVersionID, err := uuid.Parse(obj.Spec.TemplateVersionID)
	if err != nil {
		return codersdk.CreateWorkspaceRequest{}, fmt.Errorf("invalid templateVersionID %q: %w", obj.Spec.TemplateVersionID, err)
	}

	request.TemplateVersionID = templateVersionID
	return request, nil
}
