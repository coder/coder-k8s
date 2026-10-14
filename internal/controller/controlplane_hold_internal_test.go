package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #353: the grace period after the hold grows by one bounded pass per
// dependent of one kind, up to a cap, so dependents that someone keeps
// creating cannot keep a deleted control plane forever.
func TestControlPlaneDependentGraceFor(t *testing.T) {
	t.Parallel()
	require.Equal(t, 30*time.Second, controlPlaneDependentGraceFor(0))
	require.Equal(t, time.Minute, controlPlaneDependentGraceFor(3))
	require.Equal(t, 5*time.Minute, controlPlaneDependentGraceFor(1000), "the grace period has a cap")
}
