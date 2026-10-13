package controller_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

func validAIProviderSpec() coderv1alpha1.CoderAIProviderSpec {
	return coderv1alpha1.CoderAIProviderSpec{
		ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"},
		Type:            "openai",
		BaseURL:         "https://api.openai.com/v1",
		APIKeySecretRef: coderv1alpha1.SecretKeySelector{Name: "openai-api-key", Key: "api-key"},
	}
}

func TestCoderAIProviderAPIValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		name        string
		objectName  string // default: "provider"
		mutate      func(*coderv1alpha1.CoderAIProviderSpec)
		wantEnabled bool
		wantErr     string // empty: the API server accepts the object
	}{
		{name: "minimal spec", wantEnabled: true},
		{name: "full spec", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.Type, s.BaseURL = "openai-compat", "http://llm.internal:8080/v1?api-version=2"
			s.DisplayName, s.Icon, s.Enabled = strings.Repeat("d", 128), "/icon/openai.svg", ptrTo(false)
		}},
		{name: "every provider type", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.Type = "vercel"
		}, wantEnabled: true},
		{name: "name with 63 characters", objectName: strings.Repeat("a", 63), wantEnabled: true},
		{name: "name with 64 characters", objectName: strings.Repeat("a", 64), wantErr: "metadata.name is the Coder provider name"},
		{name: "name with a dot", objectName: "open.ai", wantErr: "metadata.name is the Coder provider name"},
		{name: "name with two hyphens", objectName: "open--ai", wantErr: "metadata.name is the Coder provider name"},
		{name: "unknown type", wantErr: "spec.type", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.Type = "bedrock"
		}},
		{name: "empty control plane name", wantErr: "spec.controlPlaneRef.name", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.ControlPlaneRef.Name = ""
		}},
		{name: "empty base URL", wantErr: "spec.baseURL", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.BaseURL = ""
		}},
		{name: "base URL with another scheme", wantErr: "baseURL must be an http:// or https:// URL", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.BaseURL = "ftp://api.openai.com/v1"
		}},
		{name: "base URL with user info", wantErr: "baseURL must be an http:// or https:// URL", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.BaseURL = "https://user:pass@api.openai.com/v1"
		}},
		{name: "base URL without a host", wantErr: "baseURL must be an http:// or https:// URL", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.BaseURL = "https:///v1"
		}},
		{name: "base URL with a space in the host", wantErr: "baseURL must be an http:// or https:// URL", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.BaseURL = "https://api openai.com/v1"
		}},
		{name: "base URL too long", wantErr: "spec.baseURL", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.BaseURL = "https://api.openai.com/" + strings.Repeat("v", 2026)
		}},
		{name: "empty Secret name", wantErr: "apiKeySecretRef.name must not be empty", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.APIKeySecretRef.Name = ""
		}},
		{name: "empty Secret key", wantErr: "apiKeySecretRef.key must not be empty", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.APIKeySecretRef.Key = ""
		}},
		{name: "display name too long", wantErr: "spec.displayName", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.DisplayName = strings.Repeat("d", 129)
		}},
		{name: "icon too long", wantErr: "spec.icon", mutate: func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.Icon = "/" + strings.Repeat("i", 2048)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := validAIProviderSpec()
			if tc.mutate != nil {
				tc.mutate(&spec)
			}
			// Each case gets its own namespace, so cases can share a name.
			obj := &coderv1alpha1.CoderAIProvider{
				ObjectMeta: metav1.ObjectMeta{Name: tc.objectName, Namespace: createTestNamespace(ctx, t, "aip-api")},
				Spec:       spec,
			}
			if obj.Name == "" {
				obj.Name = "provider"
			}
			err := k8sClient.Create(ctx, obj)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, obj.Spec.Enabled)
			require.Equal(t, tc.wantEnabled, *obj.Spec.Enabled)
		})
	}
}

func TestCoderAIProviderImmutableFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "aip-immutable")
	obj := &coderv1alpha1.CoderAIProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: namespace},
		Spec:       validAIProviderSpec(),
	}
	require.NoError(t, k8sClient.Create(ctx, obj))

	changed := obj.DeepCopy()
	changed.Spec.Type = "anthropic"
	require.ErrorContains(t, k8sClient.Update(ctx, changed), "type is immutable")
	changed = obj.DeepCopy()
	changed.Spec.ControlPlaneRef.Name = "other"
	require.ErrorContains(t, k8sClient.Update(ctx, changed), "controlPlaneRef is immutable")

	// The other spec fields, metadata, and status stay writable.
	obj.Spec.BaseURL, obj.Spec.DisplayName, obj.Spec.Enabled = "https://proxy.example.com/openai", "OpenAI", ptrTo(false)
	obj.Spec.APIKeySecretRef = coderv1alpha1.SecretKeySelector{Name: "openai-api-key-v2", Key: "key"}
	obj.Finalizers = []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}
	require.NoError(t, k8sClient.Update(ctx, obj))
	require.Equal(t, ptrTo(false), obj.Spec.Enabled, "enabled: false must not turn back into the default")
	obj.Status.ProviderID = "5d0c6c2e-3a4d-4c55-9d2f-3f1b1f0c9a11"
	obj.Status.Credential = &coderv1alpha1.CoderAIProviderCredentialStatus{SecretName: "openai-api-key-v2", SecretKey: "key"}
	require.NoError(t, k8sClient.Status().Update(ctx, obj))

	obj.Finalizers = nil
	require.NoError(t, k8sClient.Update(ctx, obj))
	require.NoError(t, k8sClient.Delete(ctx, obj))
}
