package controller

import (
	"context"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// This file exposes unexported helpers to the controller_test package, whose
// fake Coder serves the routes they call. It is compiled only into tests.

// MCPServerACLResult is the outcome of SyncMCPServerACL. An empty Reason
// means the ACL matches access.groups.
type MCPServerACLResult struct {
	Reason  string
	Stalled bool
	Message string
	Requeue time.Duration
}

// SyncMCPServerACL runs syncMCPServerACL.
func SyncMCPServerACL(
	ctx context.Context, sdk *codersdk.Client, orgID, configID uuid.UUID, access *coderv1alpha1.CoderMCPServerAccess,
) (MCPServerACLResult, error) {
	step, err := syncMCPServerACL(ctx, sdk, orgID, configID, access)
	if step == nil {
		return MCPServerACLResult{}, err
	}
	return MCPServerACLResult{Reason: step.reason, Stalled: step.stalled, Message: step.message, Requeue: step.requeue}, err
}
