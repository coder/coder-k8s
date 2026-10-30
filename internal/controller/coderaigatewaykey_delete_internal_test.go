package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// #384 follow-up 4 for keys: a delete pass under the control plane hold ends
// at the hold end, also when Coder does not answer, and its retry counts from
// the end of the pass. Here the hold ends in 5 s and Coder hangs on the LIST,
// whose own request timeout is 30 s.
func TestAIGatewayKeyDeleteUnderHoldEndsAtHoldEnd(t *testing.T) {
	t.Parallel()
	coder := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(coder.Close)

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, coderv1alpha1.AddToScheme(scheme))
	deleted := metav1.NewTime(time.Now().Add(5*time.Second - controlPlaneDependentHold))
	controlPlane := &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{
		Name: "coder", Namespace: "ns", UID: "cp-uid", DeletionTimestamp: &deleted, Finalizers: []string{workspaceRBACFinalizer},
	}}
	controlPlane.Status.URL = coder.URL
	controlPlane.Status.OperatorAccessReady = true
	controlPlane.Status.OperatorTokenSecretRef = &coderv1alpha1.SecretKeySelector{Name: "coder-operator-token"}
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "coder-operator-token", Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(controlPlane, coderv1alpha1.GroupVersion.WithKind("CoderControlPlane"))},
		},
		Data: map[string][]byte{coderv1alpha1.DefaultTokenSecretKey: []byte("token")},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(controlPlane, token, namespace).Build()
	r := &CoderAIGatewayKeyReconciler{Client: c, APIReader: c, Scheme: scheme, Clock: clock.RealClock{}, OperatorNamespace: "coder-system"}
	held, err := controlPlaneHolds(context.Background(), r.Client, controlPlane, time.Now())
	require.NoError(t, err)
	require.True(t, held, "assertion failed: the test needs a control plane that holds")

	key := &coderv1alpha1.CoderAIGatewayKey{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "ns", UID: testAIGatewayUID}}
	key.Spec.ControlPlaneRef.Name = "coder"
	key.Status.NextSerial = 1
	key.Status.LastCreateAttemptTime = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	start := time.Now()
	release, step, err := r.deleteStep(context.Background(), key)
	require.NoError(t, err)
	require.Less(t, time.Since(start), 15*time.Second, "the pass ends at the hold end, not at the 30 s request timeout")
	require.False(t, release, "a pass under the hold never releases without a delete")
	require.NotNil(t, step)
	// The requeue counts from the end of the pass: the time left to the hold
	// end, which is about 0 here, plus 1 s (controlPlaneHoldRequeue).
	require.LessOrEqual(t, step.requeue, 2*time.Second, "the next pass runs right after the hold end")
}
