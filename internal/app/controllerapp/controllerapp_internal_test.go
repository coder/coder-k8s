package controllerapp

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// #353: the cleanup records live in the namespace that the shipped Role
// names when neither POD_NAMESPACE nor the in-cluster namespace file says
// otherwise, also for a controller that runs outside the cluster.
func TestDetectOperatorNamespace(t *testing.T) {
	if _, err := os.Stat(inClusterNamespacePath); err == nil {
		t.Skip("this host has an in-cluster namespace file")
	}
	t.Setenv("POD_NAMESPACE", "")
	require.Equal(t, "coder-system", detectOperatorNamespace())
	require.Equal(t, "kube-system", detectLeaderElectionNamespace(), "the leader-election fallback stays")
	t.Setenv("POD_NAMESPACE", "ops")
	require.Equal(t, "ops", detectOperatorNamespace())
}
