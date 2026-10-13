package controller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

func newWorkspacePool(namespace, name string, mutate func(*coderv1alpha1.CoderWorkspacePoolSpec)) *coderv1alpha1.CoderWorkspacePool {
	pool := &coderv1alpha1.CoderWorkspacePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: coderv1alpha1.CoderWorkspacePoolSpec{
			ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"},
			OwnerUserID:     "5f0c3c4e-0000-4000-8000-000000000001",
			Template:        "default.ci-runner",
			Version:         coderv1alpha1.CoderWorkspacePoolVersion{Active: ptrTo(true)},
		},
	}
	if mutate != nil {
		mutate(&pool.Spec)
	}
	return pool
}

func TestCoderWorkspacePoolAPIValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "pool-api")
	tooManyParameters := make([]coderv1alpha1.CoderWorkspacePoolParameter, 65)
	for i := range tooManyParameters {
		tooManyParameters[i].Name = fmt.Sprintf("p%d", i)
	}

	for i, tc := range []struct {
		name    string
		mutate  func(*coderv1alpha1.CoderWorkspacePoolSpec)
		wantErr string // empty: the API server accepts the object
	}{
		{name: "minimal"},
		{name: "full spec", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
			s.Version = coderv1alpha1.CoderWorkspacePoolVersion{Name: "v1.4.0"}
			s.PresetName, s.Parameters = "small", []coderv1alpha1.CoderWorkspacePoolParameter{{Name: "region", Value: "eu-west-1"}}
			s.Replicas, s.MaxInFlight, s.ReadyTimeoutSeconds = ptrTo(int32(100)), ptrTo(int32(20)), ptrTo(int32(7200))
			s.Strategy, s.Claims.Mode = coderv1alpha1.CoderWorkspacePoolStrategyManaged, coderv1alpha1.CoderWorkspacePoolClaimModeReserve
		}},
		{name: "version by ID", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
			s.Version = coderv1alpha1.CoderWorkspacePoolVersion{ID: "5f0c3c4e-0000-4000-8000-000000000002"}
		}},
		{name: "zero replicas", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = ptrTo(int32(0)) }},
		{name: "no version", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
			s.Version = coderv1alpha1.CoderWorkspacePoolVersion{}
		}, wantErr: "set exactly one of name, id, or active"},
		{name: "two versions", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Version.Name = "v1" }, wantErr: "set exactly one of name, id, or active"},
		{name: "active false", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Version.Active = ptrTo(false) }, wantErr: "active must be true when set"},
		{name: "prebuilds strategy", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Strategy = "Prebuilds" }, wantErr: "spec.strategy"},
		{name: "share mode", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Claims.Mode = "Share" }, wantErr: "spec.claims.mode"},
		{name: "owner not a UUID", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.OwnerUserID = "alice" }, wantErr: "spec.ownerUserID"},
		{name: "template without organization", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Template = "ci-runner" }, wantErr: "spec.template"},
		{name: "reserved template name", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Template = "default.new" }, wantErr: "must not be new or create"},
		{name: "negative replicas", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = ptrTo(int32(-1)) }, wantErr: "spec.replicas"},
		{name: "too many replicas", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = ptrTo(int32(101)) }, wantErr: "spec.replicas"},
		{name: "zero in flight", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.MaxInFlight = ptrTo(int32(0)) }, wantErr: "spec.maxInFlight"},
		{name: "too many in flight", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.MaxInFlight = ptrTo(int32(21)) }, wantErr: "spec.maxInFlight"},
		{name: "short ready timeout", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.ReadyTimeoutSeconds = ptrTo(int32(59)) }, wantErr: "spec.readyTimeoutSeconds"},
		{name: "long ready timeout", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.ReadyTimeoutSeconds = ptrTo(int32(7201)) }, wantErr: "spec.readyTimeoutSeconds"},
		{name: "long preset name", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.PresetName = strings.Repeat("p", 65) }, wantErr: "spec.presetName"},
		{name: "too many parameters", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Parameters = tooManyParameters }, wantErr: "spec.parameters"},
		{name: "duplicate parameter", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
			s.Parameters = []coderv1alpha1.CoderWorkspacePoolParameter{{Name: "region"}, {Name: "region"}}
		}, wantErr: "Duplicate value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := k8sClient.Create(ctx, newWorkspacePool(namespace, fmt.Sprintf("pool-%d", i), tc.mutate))
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.True(t, apierrors.IsInvalid(err), "want Invalid, got %v", err)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestCoderWorkspacePoolAPIDefaultsAndImmutability(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "pool-api")
	pool := newWorkspacePool(namespace, "ci-runners", nil)
	require.NoError(t, k8sClient.Create(ctx, pool))

	got := &coderv1alpha1.CoderWorkspacePool{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), got))
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolStrategyManaged, got.Spec.Strategy)
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolClaimModeReserve, got.Spec.Claims.Mode)
	require.Equal(t, int32(1), *got.Spec.Replicas)
	require.Equal(t, int32(5), *got.Spec.MaxInFlight)
	require.Equal(t, int32(900), *got.Spec.ReadyTimeoutSeconds)

	for _, tc := range []struct {
		name    string
		mutate  func(*coderv1alpha1.CoderWorkspacePoolSpec)
		wantErr string
	}{
		{name: "control plane", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.ControlPlaneRef.Name = "other" }, wantErr: "controlPlaneRef is immutable"},
		{name: "owner", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
			s.OwnerUserID = "5f0c3c4e-0000-4000-8000-000000000009"
		}, wantErr: "ownerUserID is immutable"},
		{name: "template", mutate: func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Template = "default.other" }, wantErr: "template is immutable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := got.DeepCopy()
			tc.mutate(&changed.Spec)
			err := k8sClient.Update(ctx, changed)
			require.True(t, apierrors.IsInvalid(err), "want Invalid, got %v", err)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}

	// The version, the preset, the parameters, and the sizes stay mutable.
	changed := got.DeepCopy()
	changed.Spec.Version = coderv1alpha1.CoderWorkspacePoolVersion{Name: "v2"}
	changed.Spec.PresetName, changed.Spec.Parameters = "large", []coderv1alpha1.CoderWorkspacePoolParameter{{Name: "region", Value: "us-east-1"}}
	changed.Spec.Replicas, changed.Spec.MaxInFlight, changed.Spec.ReadyTimeoutSeconds = ptrTo(int32(0)), ptrTo(int32(1)), ptrTo(int32(60))
	require.NoError(t, k8sClient.Update(ctx, changed))
}

// TestCoderWorkspacePoolScaleSubresource proves the contract that kubectl
// scale, HPA, and KEDA rely on: GET /scale never fails, a /scale write runs
// the CRD schema and bumps the generation, and status feeds the scale status.
func TestCoderWorkspacePoolScaleSubresource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "pool-api")
	pool := newWorkspacePool(namespace, "ci-runners", nil)
	require.NoError(t, k8sClient.Create(ctx, pool))
	generation := pool.Generation

	scale := &autoscalingv1.Scale{}
	require.NoError(t, k8sClient.SubResource("scale").Get(ctx, pool, scale))
	require.Equal(t, int32(1), scale.Spec.Replicas, "the schema default feeds GET /scale")
	require.Zero(t, scale.Status.Replicas)
	require.Empty(t, scale.Status.Selector)

	scale.Spec.Replicas = 3
	require.NoError(t, k8sClient.SubResource("scale").Update(ctx, pool, client.WithSubResourceBody(scale)))
	got := &coderv1alpha1.CoderWorkspacePool{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), got))
	require.Equal(t, int32(3), *got.Spec.Replicas)
	require.Equal(t, generation+1, got.Generation, "a /scale write bumps metadata.generation")

	// The schema maximum binds /scale too, so autoscalers cannot pass it.
	require.NoError(t, k8sClient.SubResource("scale").Get(ctx, pool, scale))
	scale.Spec.Replicas = 101
	err := k8sClient.SubResource("scale").Update(ctx, pool, client.WithSubResourceBody(scale))
	require.True(t, apierrors.IsInvalid(err), "want Invalid, got %v", err)
	require.ErrorContains(t, err, "spec.replicas")

	// A stale resourceVersion conflicts, as kubectl scale --current-replicas expects.
	stale := scale.DeepCopy()
	scale.Spec.Replicas = 0
	require.NoError(t, k8sClient.SubResource("scale").Update(ctx, pool, client.WithSubResourceBody(scale)))
	stale.Spec.Replicas = 2
	err = k8sClient.SubResource("scale").Update(ctx, pool, client.WithSubResourceBody(stale))
	require.True(t, apierrors.IsConflict(err), "want Conflict, got %v", err)

	// Status feeds the scale status.
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), got))
	got.Status.Replicas = 2
	got.Status.Selector = coderv1alpha1.CoderWorkspacePoolSelectorLabel + "=" + string(got.UID)
	require.NoError(t, k8sClient.Status().Update(ctx, got))
	require.NoError(t, k8sClient.SubResource("scale").Get(ctx, pool, scale))
	require.Equal(t, int32(0), scale.Spec.Replicas)
	require.Equal(t, int32(2), scale.Status.Replicas)
	require.Equal(t, got.Status.Selector, scale.Status.Selector)
}
