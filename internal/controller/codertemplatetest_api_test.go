package controller_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

func validTemplateTestSpec() coderv1alpha1.CoderTemplateTestSpec {
	return coderv1alpha1.CoderTemplateTestSpec{
		ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"},
		Template:        "default.docker",
		Version:         coderv1alpha1.CoderTemplateTestVersion{Name: "v1.4.0"},
	}
}

func TestCoderTemplateTestAPIValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "ktt-api")

	for _, tc := range []struct {
		name    string
		mutate  func(*coderv1alpha1.CoderTemplateTestSpec)
		wantErr string // empty: the API server accepts the object
	}{
		{name: "version name", mutate: func(*coderv1alpha1.CoderTemplateTestSpec) {}},
		{name: "version id", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Version = coderv1alpha1.CoderTemplateTestVersion{ID: "0b6f6c2e-3a4d-4c55-9d2f-3f1b1f0c9a11"}
		}},
		{name: "version active", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Version = coderv1alpha1.CoderTemplateTestVersion{Active: ptrTo(true)}
		}},
		{name: "full spec", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Parameters = []coderv1alpha1.CoderTemplateTestParameter{{Name: "region", Value: "eu-west-1"}, {Name: "empty"}}
			s.TimeoutSeconds, s.TTLSecondsAfterFinished = ptrTo[int32](7200), ptrTo[int32](0)
		}},
		{name: "no version", wantErr: "set exactly one of name, id, or active", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Version = coderv1alpha1.CoderTemplateTestVersion{}
		}},
		{name: "two versions", wantErr: "set exactly one of name, id, or active", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Version.Active = ptrTo(true)
		}},
		{name: "active false", wantErr: "active must be true when set", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Version = coderv1alpha1.CoderTemplateTestVersion{Active: ptrTo(false)}
		}},
		{name: "bad id", wantErr: "spec.version.id", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Version = coderv1alpha1.CoderTemplateTestVersion{ID: "not-a-uuid"}
		}},
		{name: "template without organization", wantErr: "spec.template", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Template = "docker"
		}},
		{name: "template with three segments", wantErr: "spec.template", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Template = "default.docker.v1"
		}},
		{name: "template too long", wantErr: "spec.template", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Template = strings.Repeat("a", 33) + "." + strings.Repeat("b", 32)
		}},
		{name: "empty control plane name", wantErr: "spec.controlPlaneRef.name", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.ControlPlaneRef.Name = ""
		}},
		{name: "control plane name not a DNS name", wantErr: "spec.controlPlaneRef.name", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.ControlPlaneRef.Name = "not a name"
		}},
		{name: "organization name too long", wantErr: "organization and template names must each have at most 32 characters", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Template = strings.Repeat("a", 33) + ".b"
		}},
		{name: "reserved template name", wantErr: "organization and template names must not be new or create", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Template = "default.new"
		}},
		{name: "reserved organization name", wantErr: "organization and template names must not be new or create", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Template = "create.docker"
		}},
		{name: "reserved word inside a name", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Template = "newco.create-env"
		}},
		{name: "version name with a slash", wantErr: "spec.version.name", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Version.Name = "v1/../v2"
		}},
		{name: "version name with a leading dot", wantErr: "spec.version.name", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Version.Name = ".v1"
		}},
		{name: "duplicate parameter", wantErr: "spec.parameters", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.Parameters = []coderv1alpha1.CoderTemplateTestParameter{{Name: "a"}, {Name: "a"}}
		}},
		{name: "too many parameters", wantErr: "spec.parameters", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			for i := range 65 {
				s.Parameters = append(s.Parameters, coderv1alpha1.CoderTemplateTestParameter{Name: strings.Repeat("p", i+1)})
			}
		}},
		{name: "timeout too short", wantErr: "spec.timeoutSeconds", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.TimeoutSeconds = ptrTo[int32](59)
		}},
		{name: "timeout too long", wantErr: "spec.timeoutSeconds", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.TimeoutSeconds = ptrTo[int32](7201)
		}},
		{name: "negative ttl", wantErr: "spec.ttlSecondsAfterFinished", mutate: func(s *coderv1alpha1.CoderTemplateTestSpec) {
			s.TTLSecondsAfterFinished = ptrTo[int32](-1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := validTemplateTestSpec()
			tc.mutate(&spec)
			obj := &coderv1alpha1.CoderTemplateTest{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "ktt-", Namespace: namespace},
				Spec:       spec,
			}
			wantTimeout := int32(900) // The API server default.
			if spec.TimeoutSeconds != nil {
				wantTimeout = *spec.TimeoutSeconds
			}
			err := k8sClient.Create(ctx, obj)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, obj.Spec.TimeoutSeconds)
			require.Equal(t, wantTimeout, *obj.Spec.TimeoutSeconds)
		})
	}
}

func TestCoderTemplateTestSpecIsImmutable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "ktt-immutable")
	obj := &coderv1alpha1.CoderTemplateTest{
		ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: namespace},
		Spec:       validTemplateTestSpec(),
	}
	require.NoError(t, k8sClient.Create(ctx, obj))
	require.Equal(t, ptrTo[int32](900), obj.Spec.TimeoutSeconds)

	changed := obj.DeepCopy()
	changed.Spec.TimeoutSeconds = ptrTo[int32](600)
	require.ErrorContains(t, k8sClient.Update(ctx, changed), "spec is immutable, create a new CoderTemplateTest")

	// Metadata and status stay writable.
	obj.Labels = map[string]string{"run": "nightly"}
	obj.Finalizers = []string{"coder.com/template-test-cleanup"}
	require.NoError(t, k8sClient.Update(ctx, obj))
	obj.Status.Phase = coderv1alpha1.CoderTemplateTestPhasePending
	obj.Status.Reason = "OwnerNotConfigured"
	require.NoError(t, k8sClient.Status().Update(ctx, obj))

	obj.Finalizers = nil
	require.NoError(t, k8sClient.Update(ctx, obj))
	require.NoError(t, k8sClient.Delete(ctx, obj))
}

// Setting the tester owner must not roll the Coder Deployment (plan risk R5).
func TestTemplateTestsOwnerLeavesControlPlaneDeploymentUnchanged(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "ktt-owner")
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: namespace},
		Spec:       coderv1alpha1.CoderControlPlaneSpec{Image: "img:v1"},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	key := types.NamespacedName{Name: cp.Name, Namespace: namespace}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	before := &appsv1.Deployment{}
	require.NoError(t, k8sClient.Get(ctx, key, before))

	require.NoError(t, k8sClient.Get(ctx, key, cp))
	cp.Spec.TemplateTests = &coderv1alpha1.TemplateTestsSpec{OwnerUserID: "not-a-uuid"}
	require.ErrorContains(t, k8sClient.Update(ctx, cp), "spec.templateTests.ownerUserID")
	cp.Spec.TemplateTests.OwnerUserID = "0b6f6c2e-3a4d-4c55-9d2f-3f1b1f0c9a11"
	require.NoError(t, k8sClient.Update(ctx, cp))
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	after := &appsv1.Deployment{}
	require.NoError(t, k8sClient.Get(ctx, key, after))
	require.True(t, apiequality.Semantic.DeepEqual(before.Spec.Template, after.Spec.Template), "pod template changed")
	require.Equal(t, before.Generation, after.Generation)
}
