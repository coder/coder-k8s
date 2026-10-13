package controller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const prebuildsUserID = "c42fdf75-3097-471c-8c33-fb52454d81c0"

func newPoolMember(namespace, name string, mutate func(*coderv1alpha1.CoderWorkspacePoolMemberSpec)) *coderv1alpha1.CoderWorkspacePoolMember {
	member := &coderv1alpha1.CoderWorkspacePoolMember{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: coderv1alpha1.CoderWorkspacePoolMemberSpec{
			ControlPlaneRef:     coderv1alpha1.CoderControlPlaneReference{Name: "coder"},
			PoolName:            "ci-runners",
			OwnerID:             "5f0c3c4e-0000-4000-8000-000000000001",
			OrganizationID:      "5f0c3c4e-0000-4000-8000-000000000002",
			TemplateID:          "5f0c3c4e-0000-4000-8000-000000000003",
			TemplateVersionID:   "5f0c3c4e-0000-4000-8000-000000000004",
			ParametersHash:      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			ReadyTimeoutSeconds: 900,
		},
	}
	if mutate != nil {
		mutate(&member.Spec)
	}
	return member
}

func requireInvalid(t *testing.T, err error, want string) {
	t.Helper()
	require.True(t, apierrors.IsInvalid(err), "want Invalid, got %v", err)
	require.ErrorContains(t, err, want)
}

func TestCoderWorkspacePoolMemberAPIValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "member-api")
	type spec = coderv1alpha1.CoderWorkspacePoolMemberSpec
	upper := strings.ToUpper("5f0c3c4e-0000-4000-8000-0000000000aa")

	for i, tc := range []struct {
		name    string
		mutate  func(*spec)
		wantErr string // empty: the API server accepts the object
	}{
		{name: "minimal"},
		{name: "full spec", mutate: func(s *spec) {
			s.PresetID = "5f0c3c4e-0000-4000-8000-000000000005"
			s.Parameters = []coderv1alpha1.CoderWorkspacePoolParameter{{Name: "region", Value: "eu-west-1"}}
			s.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: "build-1234", UID: "5f0c3c4e-0000-4000-8000-000000000006"}
		}},
		{name: "prebuilds owner", mutate: func(s *spec) { s.OwnerID = prebuildsUserID }, wantErr: "ownerID must not be the Coder prebuilds system user"},
		{name: "owner not a UUID", mutate: func(s *spec) { s.OwnerID = "alice" }, wantErr: "spec.ownerID"},
		{name: "owner in upper case", mutate: func(s *spec) { s.OwnerID = upper }, wantErr: "spec.ownerID"},
		{name: "organization in upper case", mutate: func(s *spec) { s.OrganizationID = upper }, wantErr: "spec.organizationID"},
		{name: "template without hyphens", mutate: func(s *spec) { s.TemplateID = strings.ReplaceAll(s.TemplateID, "-", "") }, wantErr: "spec.templateID"},
		{name: "version in upper case", mutate: func(s *spec) { s.TemplateVersionID = upper }, wantErr: "spec.templateVersionID"},
		{name: "preset in upper case", mutate: func(s *spec) { s.PresetID = upper }, wantErr: "spec.presetID"},
		{name: "claim UID in upper case", mutate: func(s *spec) {
			s.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: "build-1234", UID: upper}
		}, wantErr: "spec.claimRef.uid"},
		{name: "claim name not a DNS name", mutate: func(s *spec) {
			s.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: "Build_1234", UID: "5f0c3c4e-0000-4000-8000-000000000006"}
		}, wantErr: "spec.claimRef.name"},
		{name: "pool name not a DNS name", mutate: func(s *spec) { s.PoolName = "CI" }, wantErr: "spec.poolName"},
		{name: "no parameters hash", mutate: func(s *spec) { s.ParametersHash = "" }, wantErr: "spec.parametersHash"},
		{name: "short ready timeout", mutate: func(s *spec) { s.ReadyTimeoutSeconds = 59 }, wantErr: "spec.readyTimeoutSeconds"},
		{name: "long ready timeout", mutate: func(s *spec) { s.ReadyTimeoutSeconds = 7201 }, wantErr: "spec.readyTimeoutSeconds"},
		{name: "duplicate parameter", mutate: func(s *spec) {
			s.Parameters = []coderv1alpha1.CoderWorkspacePoolParameter{{Name: "region"}, {Name: "region"}}
		}, wantErr: "Duplicate value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := k8sClient.Create(ctx, newPoolMember(namespace, fmt.Sprintf("member-%d", i), tc.mutate))
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			requireInvalid(t, err, tc.wantErr)
		})
	}

	t.Run("no spec", func(t *testing.T) {
		member := &unstructured.Unstructured{}
		member.SetGroupVersionKind(coderv1alpha1.GroupVersion.WithKind("CoderWorkspacePoolMember"))
		member.SetNamespace(namespace)
		member.SetName("no-spec")
		requireInvalid(t, k8sClient.Create(ctx, member), "spec: Required value")
	})
	// A stored empty list would make every later typed update look like a
	// parameters change, the claimRef bind included.
	t.Run("empty parameters list", func(t *testing.T) {
		member := &unstructured.Unstructured{}
		member.SetGroupVersionKind(coderv1alpha1.GroupVersion.WithKind("CoderWorkspacePoolMember"))
		member.SetNamespace(namespace)
		member.SetName("empty-parameters")
		spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&newPoolMember(namespace, "", nil).Spec)
		require.NoError(t, err)
		spec["parameters"] = []any{}
		require.NoError(t, unstructured.SetNestedMap(member.Object, spec, "spec"))
		requireInvalid(t, k8sClient.Create(ctx, member), "spec.parameters")
	})
	t.Run("unknown phase", func(t *testing.T) {
		member := newPoolMember(namespace, "phase", nil)
		require.NoError(t, k8sClient.Create(ctx, member))
		member.Status.Phase = "Bound"
		requireInvalid(t, k8sClient.Status().Update(ctx, member), "status.phase")
	})
}

// TestCoderWorkspacePoolMemberAPIImmutability proves the contract that the
// identity guard relies on: the pinned inputs never change, and claimRef can
// be set once.
func TestCoderWorkspacePoolMemberAPIImmutability(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "member-api")
	type spec = coderv1alpha1.CoderWorkspacePoolMemberSpec
	member := newPoolMember(namespace, "member", func(s *spec) {
		s.Parameters = []coderv1alpha1.CoderWorkspacePoolParameter{{Name: "region", Value: "eu-west-1"}}
	})
	require.NoError(t, k8sClient.Create(ctx, member))
	other := "5f0c3c4e-0000-4000-8000-000000000009"

	update := func(mutate func(*spec)) error {
		changed := &coderv1alpha1.CoderWorkspacePoolMember{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(member), changed))
		mutate(&changed.Spec)
		return k8sClient.Update(ctx, changed)
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*spec)
		wantErr string
	}{
		{name: "control plane", mutate: func(s *spec) { s.ControlPlaneRef.Name = "other" }, wantErr: "controlPlaneRef is immutable"},
		{name: "pool", mutate: func(s *spec) { s.PoolName = "other" }, wantErr: "poolName is immutable"},
		{name: "owner", mutate: func(s *spec) { s.OwnerID = other }, wantErr: "ownerID is immutable"},
		{name: "organization", mutate: func(s *spec) { s.OrganizationID = other }, wantErr: "organizationID is immutable"},
		{name: "template", mutate: func(s *spec) { s.TemplateID = other }, wantErr: "templateID is immutable"},
		{name: "version", mutate: func(s *spec) { s.TemplateVersionID = other }, wantErr: "templateVersionID is immutable"},
		{name: "add a preset", mutate: func(s *spec) { s.PresetID = other }, wantErr: "presetID is immutable"},
		{name: "change a parameter", mutate: func(s *spec) { s.Parameters[0].Value = "us-east-1" }, wantErr: "parameters are immutable"},
		{name: "remove the parameters", mutate: func(s *spec) { s.Parameters = nil }, wantErr: "parameters are immutable"},
		{name: "parameters hash", mutate: func(s *spec) { s.ParametersHash = "other" }, wantErr: "parametersHash is immutable"},
		{name: "ready timeout", mutate: func(s *spec) { s.ReadyTimeoutSeconds = 60 }, wantErr: "readyTimeoutSeconds is immutable"},
	} {
		t.Run(tc.name, func(t *testing.T) { requireInvalid(t, update(tc.mutate), tc.wantErr) })
	}

	claim := coderv1alpha1.CoderWorkspaceClaimReference{Name: "build-1234", UID: "5f0c3c4e-0000-4000-8000-000000000006"}
	require.NoError(t, update(func(s *spec) { s.ClaimRef = &claim }), "the pool sets claimRef once")
	requireInvalid(t, update(func(s *spec) { s.ClaimRef.UID = other }), "claimRef can be set once")
	requireInvalid(t, update(func(s *spec) { s.ClaimRef.Name = "other" }), "claimRef can be set once")
	requireInvalid(t, update(func(s *spec) { s.ClaimRef = nil }), "claimRef can be set once")
}
