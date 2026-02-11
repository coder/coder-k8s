package convert

import (
	"strconv"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// WorkspaceToK8s converts a codersdk.Workspace to an aggregated API CoderWorkspace.
func WorkspaceToK8s(namespace string, w codersdk.Workspace) *aggregationv1alpha1.CoderWorkspace {
	if namespace == "" {
		panic("assertion failed: namespace must not be empty")
	}

	lastUsedAt := metav1.NewTime(w.LastUsedAt)

	return &aggregationv1alpha1.CoderWorkspace{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CoderWorkspace",
			APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              coder.BuildWorkspaceName(w.OrganizationName, w.OwnerName, w.Name),
			Namespace:         namespace,
			UID:               types.UID(w.ID.String()),
			ResourceVersion:   strconv.FormatInt(w.UpdatedAt.UnixNano(), 10),
			CreationTimestamp: metav1.NewTime(w.CreatedAt),
		},
		Spec: aggregationv1alpha1.CoderWorkspaceSpec{
			Organization:      w.OrganizationName,
			TemplateName:      w.TemplateName,
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
			LastUsedAt:        &lastUsedAt,
		},
	}
}

func workspaceRunning(workspace codersdk.Workspace) bool {
	running := workspace.LatestBuild.Transition == codersdk.WorkspaceTransitionStart
	if workspace.LatestBuild.Status == codersdk.WorkspaceStatusRunning {
		running = true
	}

	return running
}

// WorkspaceCreateRequestFromK8s builds a codersdk.CreateWorkspaceRequest.
func WorkspaceCreateRequestFromK8s(
	obj *aggregationv1alpha1.CoderWorkspace,
	workspaceName string,
	templateID uuid.UUID,
) codersdk.CreateWorkspaceRequest {
	if obj == nil {
		panic("assertion failed: workspace object must not be nil")
	}
	if workspaceName == "" {
		panic("assertion failed: workspace name must not be empty")
	}
	if templateID == uuid.Nil {
		panic("assertion failed: template ID must not be nil")
	}

	return codersdk.CreateWorkspaceRequest{
		Name:              workspaceName,
		TemplateID:        templateID,
		TTLMillis:         obj.Spec.TTLMillis,
		AutostartSchedule: obj.Spec.AutostartSchedule,
	}
}
