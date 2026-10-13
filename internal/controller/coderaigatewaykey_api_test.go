package controller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

func validAIGatewayKeySpec() coderv1alpha1.CoderAIGatewayKeySpec {
	return coderv1alpha1.CoderAIGatewayKeySpec{
		ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"},
		SecretName:      "coder-ai-gateway-key",
	}
}

func TestCoderAIGatewayKeyAPIValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type rotation struct{ interval, grace, consumerWait int32 }
	defaults := rotation{interval: 2592000, grace: 3600, consumerWait: 86400}
	for _, tc := range []struct {
		name    string
		mutate  func(*coderv1alpha1.CoderAIGatewayKeySpec)
		want    rotation // checked when wantErr is empty
		wantErr string   // empty: the API server accepts the object
	}{
		{name: "minimal spec gets the rotation defaults", want: defaults},
		{name: "full spec", want: rotation{interval: 0, grace: 604800, consumerWait: 0}, mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation = coderv1alpha1.CoderAIGatewayKeyRotationSpec{
				IntervalSeconds: ptrTo(int32(0)), GracePeriodSeconds: ptrTo(int32(604800)),
				ConsumerWaitSeconds: ptrTo(int32(0)), Trigger: strings.Repeat("t", 63),
			}
		}},
		{name: "shortest interval with a grace period just below it", want: rotation{interval: 86400, grace: 86399, consumerWait: 86400}, mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.IntervalSeconds, s.Rotation.GracePeriodSeconds = ptrTo(int32(86400)), ptrTo(int32(86399))
		}},
		{name: "longest interval", want: rotation{interval: 31536000, grace: 3600, consumerWait: 86400}, mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.IntervalSeconds = ptrTo(int32(31536000))
		}},
		{name: "Secret name with dots and 253 characters", want: defaults, mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.SecretName = strings.Repeat("a", 125) + "." + strings.Repeat("b", 127)
		}},
		{name: "empty control plane name", wantErr: "spec.controlPlaneRef.name", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.ControlPlaneRef.Name = ""
		}},
		{name: "empty Secret name", wantErr: "spec.secretName", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.SecretName = ""
		}},
		{name: "Secret name with uppercase letters", wantErr: "spec.secretName", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.SecretName = "Gateway-Key"
		}},
		{name: "Secret name with 254 characters", wantErr: "spec.secretName", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.SecretName = strings.Repeat("a", 254)
		}},
		{name: "interval below one day", wantErr: "intervalSeconds must be 0 or at least 86400", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.IntervalSeconds = ptrTo(int32(86399))
		}},
		{name: "negative interval", wantErr: "spec.rotation.intervalSeconds", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.IntervalSeconds = ptrTo(int32(-1))
		}},
		{name: "interval above one year", wantErr: "spec.rotation.intervalSeconds", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.IntervalSeconds = ptrTo(int32(31536001))
		}},
		{name: "grace period below ten minutes", wantErr: "spec.rotation.gracePeriodSeconds", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.GracePeriodSeconds = ptrTo(int32(599))
		}},
		{name: "grace period above seven days", wantErr: "spec.rotation.gracePeriodSeconds", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.GracePeriodSeconds = ptrTo(int32(604801))
		}},
		{name: "grace period equal to the interval", wantErr: "gracePeriodSeconds must be shorter than intervalSeconds", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.IntervalSeconds, s.Rotation.GracePeriodSeconds = ptrTo(int32(86400)), ptrTo(int32(86400))
		}},
		{name: "grace period longer than the interval", wantErr: "gracePeriodSeconds must be shorter than intervalSeconds", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.IntervalSeconds, s.Rotation.GracePeriodSeconds = ptrTo(int32(100000)), ptrTo(int32(200000))
		}},
		{name: "negative consumer wait", wantErr: "spec.rotation.consumerWaitSeconds", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.ConsumerWaitSeconds = ptrTo(int32(-1))
		}},
		{name: "consumer wait above seven days", wantErr: "spec.rotation.consumerWaitSeconds", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.ConsumerWaitSeconds = ptrTo(int32(604801))
		}},
		{name: "trigger too long", wantErr: "spec.rotation.trigger", mutate: func(s *coderv1alpha1.CoderAIGatewayKeySpec) {
			s.Rotation.Trigger = strings.Repeat("t", 64)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := validAIGatewayKeySpec()
			if tc.mutate != nil {
				tc.mutate(&spec)
			}
			// Each case gets its own namespace, so cases can share a name.
			obj := &coderv1alpha1.CoderAIGatewayKey{
				ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: createTestNamespace(ctx, t, "aigk-api")},
				Spec:       spec,
			}
			err := k8sClient.Create(ctx, obj)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			got := obj.Spec.Rotation
			require.NotNil(t, got.IntervalSeconds)
			require.NotNil(t, got.GracePeriodSeconds)
			require.NotNil(t, got.ConsumerWaitSeconds)
			require.Equal(t, tc.want, rotation{interval: *got.IntervalSeconds, grace: *got.GracePeriodSeconds, consumerWait: *got.ConsumerWaitSeconds})
		})
	}
}

// A manifest without spec.rotation, as kubectl sends it, gets every default.
// The Go client always sends rotation: {}, so this needs an unstructured
// object.
func TestCoderAIGatewayKeyRotationDefaultsWithoutRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": coderv1alpha1.GroupVersion.String(),
		"kind":       "CoderAIGatewayKey",
		"metadata":   map[string]any{"name": "gateway", "namespace": createTestNamespace(ctx, t, "aigk-defaults")},
		"spec":       map[string]any{"controlPlaneRef": map[string]any{"name": "coder"}, "secretName": "coder-ai-gateway-key"},
	}}
	require.NoError(t, k8sClient.Create(ctx, u))

	got := &coderv1alpha1.CoderAIGatewayKey{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(u), got))
	require.Equal(t, ptrTo(int32(2592000)), got.Spec.Rotation.IntervalSeconds)
	require.Equal(t, ptrTo(int32(3600)), got.Spec.Rotation.GracePeriodSeconds)
	require.Equal(t, ptrTo(int32(86400)), got.Spec.Rotation.ConsumerWaitSeconds)
}

func TestCoderAIGatewayKeyImmutableFieldsAndStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "aigk-immutable")
	obj := &coderv1alpha1.CoderAIGatewayKey{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: namespace},
		Spec:       validAIGatewayKeySpec(),
	}
	require.NoError(t, k8sClient.Create(ctx, obj))

	changed := obj.DeepCopy()
	changed.Spec.SecretName = "other-secret"
	require.ErrorContains(t, k8sClient.Update(ctx, changed), "secretName is immutable")
	changed = obj.DeepCopy()
	changed.Spec.ControlPlaneRef.Name = "other"
	require.ErrorContains(t, k8sClient.Update(ctx, changed), "controlPlaneRef is immutable")

	// Rotation settings and metadata stay writable. An explicit 0 must not
	// turn back into the default.
	obj.Spec.Rotation.IntervalSeconds, obj.Spec.Rotation.ConsumerWaitSeconds = ptrTo(int32(0)), ptrTo(int32(0))
	obj.Spec.Rotation.Trigger = "rotate-1"
	obj.Finalizers = []string{coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer}
	require.NoError(t, k8sClient.Update(ctx, obj))
	require.Equal(t, ptrTo(int32(0)), obj.Spec.Rotation.IntervalSeconds)
	require.Equal(t, ptrTo(int32(0)), obj.Spec.Rotation.ConsumerWaitSeconds)

	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	retiring := func(serial int64) coderv1alpha1.CoderAIGatewayKeyRetiring {
		return coderv1alpha1.CoderAIGatewayKeyRetiring{
			Serial: serial, ID: fmt.Sprintf("7d1e0c2e-3a4d-4c55-9d2f-3f1b1f0c9a%02d", serial),
			Name: fmt.Sprintf("ck8s-0123456789abcdef0123456789abcdef-%d", serial), ReplacedAt: now,
		}
	}
	obj.Status = coderv1alpha1.CoderAIGatewayKeyStatus{
		NextSerial:            5,
		LastCreateAttemptTime: &now,
		PendingKey:            &coderv1alpha1.CoderAIGatewayKeyPending{Serial: 4, Name: "ck8s-0123456789abcdef0123456789abcdef-4", CreateAttemptTime: now},
		CurrentKey: &coderv1alpha1.CoderAIGatewayKeyCurrent{
			Serial: 3, ID: "1b2c0c2e-3a4d-4c55-9d2f-3f1b1f0c9a11", Name: "ck8s-0123456789abcdef0123456789abcdef-3", IssuedAt: now,
		},
		RetiringKeys:       []coderv1alpha1.CoderAIGatewayKeyRetiring{retiring(0), retiring(1), retiring(2)},
		LastHandledTrigger: "rotate-1",
		NextRotationTime:   &now,
	}
	require.NoError(t, k8sClient.Status().Update(ctx, obj))

	tooMany := obj.DeepCopy()
	tooMany.Status.RetiringKeys = append(tooMany.Status.RetiringKeys, retiring(5))
	require.ErrorContains(t, k8sClient.Status().Update(ctx, tooMany), "status.retiringKeys")
	badID := obj.DeepCopy()
	badID.Status.CurrentKey.ID = "not-a-uuid"
	require.ErrorContains(t, k8sClient.Status().Update(ctx, badID), "status.currentKey.id")

	obj.Finalizers = nil
	require.NoError(t, k8sClient.Update(ctx, obj))
	require.NoError(t, k8sClient.Delete(ctx, obj))
}
