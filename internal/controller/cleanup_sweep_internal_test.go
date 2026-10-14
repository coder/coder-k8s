package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/event"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// #353: a control plane wakes the cleanup records when it gets operator
// access or a new URL, since it can then match their deployment.
func TestOperatorAccessGained(t *testing.T) {
	t.Parallel()
	cp := func(ready bool, url string) *coderv1alpha1.CoderControlPlane {
		return &coderv1alpha1.CoderControlPlane{Status: coderv1alpha1.CoderControlPlaneStatus{OperatorAccessReady: ready, URL: url}}
	}
	require.True(t, operatorAccessGained.Create(event.CreateEvent{Object: cp(true, "http://a")}))
	require.False(t, operatorAccessGained.Create(event.CreateEvent{Object: cp(false, "http://a")}))
	require.True(t, operatorAccessGained.Update(event.UpdateEvent{ObjectOld: cp(false, "http://a"), ObjectNew: cp(true, "http://a")}))
	require.True(t, operatorAccessGained.Update(event.UpdateEvent{ObjectOld: cp(true, "http://a"), ObjectNew: cp(true, "http://b")}))
	require.False(t, operatorAccessGained.Update(event.UpdateEvent{ObjectOld: cp(true, "http://a"), ObjectNew: cp(true, "http://a")}), "no wake on every status write")
	require.False(t, operatorAccessGained.Update(event.UpdateEvent{ObjectOld: cp(true, "http://a"), ObjectNew: cp(false, "http://a")}))
	require.False(t, operatorAccessGained.Delete(event.DeleteEvent{Object: cp(true, "http://a")}))
}
