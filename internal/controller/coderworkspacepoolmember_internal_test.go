package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

func TestUIDWorkspaceNameNeedsFourCharacterPrefix(t *testing.T) {
	t.Parallel()
	uid := types.UID("5f0c3c4e-0000-4000-8000-000000000001")
	name, err := uidWorkspaceName("kwp-", uid)
	require.NoError(t, err)
	require.Equal(t, "kwp-5f0c3c4e00004000800000000000", name)
	require.Len(t, name, 32, "Coder's limit")
	for _, prefix := range []string{"", "kw-", "kwpx-"} {
		_, err := uidWorkspaceName(prefix, uid)
		require.ErrorContains(t, err, "assertion failed: workspace name prefix", prefix)
	}
}

// TestWorkspacePoolConsentComparesUUIDs: the owner match compares parsed
// UUIDs, so a spelling difference never decides consent.
func TestWorkspacePoolConsentComparesUUIDs(t *testing.T) {
	t.Parallel()
	const owner = "5f0c3c4e-0000-4000-8000-0000000000aa"
	controlPlane := func(userID string) *coderv1alpha1.CoderControlPlane {
		cp := &coderv1alpha1.CoderControlPlane{}
		cp.Spec.WorkspacePools = &coderv1alpha1.WorkspacePoolsSpec{Owners: []coderv1alpha1.WorkspacePoolOwner{{UserID: userID, Templates: []string{"default.docker"}}}}
		return cp
	}
	require.Nil(t, workspacePoolConsent(controlPlane(strings.ToUpper(owner)), owner, "default.docker"))
	require.Nil(t, workspacePoolConsent(controlPlane(owner), strings.ToUpper(owner), "default.docker"))
	for _, other := range []string{"5f0c3c4e-0000-4000-8000-0000000000ab", "not-a-uuid", ""} {
		step := workspacePoolConsent(controlPlane(other), owner, "default.docker")
		require.NotNil(t, step, other)
		require.Equal(t, "OwnerNotAllowed", step.reason)
	}
}
