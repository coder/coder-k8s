package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// The controller repeats Coder's base_url check on the exact spec value.
// Admission also rejects most of these, so they are tested here.
func TestAIProviderBaseURL(t *testing.T) {
	t.Parallel()
	for raw, wantErr := range map[string]bool{
		"https://api.openai.com/v1":          false,
		"http://[fd00::1]:8080/v1#models":    false,
		"https://api.example.com/v1?x=%zz":   false, // Coder's url.Parse accepts a bad query escape too.
		"https://api.example.com/v1#%zz":     true,  // Bad escape in the fragment.
		"https://api.example.com/v1#a%2":     true,
		"https://api%.example.com":           true,
		"https://api.example.com/%zz":        true,
		"https://api.openai.com/v1 ":         true, // The controller never trims.
		"http://:8080/v1":                    true, // No host name.
		"ftp://api.openai.com":               true,
		"api.openai.com":                     true,
		"https://user:pass@api.openai.com/v": true, // Coder accepts user info. Admission and the controller refuse it.
	} {
		_, err := aiProviderBaseURL(raw)
		require.Equal(t, wantErr, err != nil, "%q: %v", raw, err)
	}
}

// The status message for a bad base URL is fixed: the parse error repeats
// the URL, which can hold user info.
func TestAIProviderBadBaseURLMessage(t *testing.T) {
	t.Parallel()
	r := &CoderAIProviderReconciler{}
	provider := &coderv1alpha1.CoderAIProvider{}
	provider.Spec.BaseURL = "https://user:hunter2-password@api%.example.com/v1"
	step, err := r.check(context.Background(), provider, provider.Status.DeepCopy())
	require.NoError(t, err)
	require.True(t, step.stalled)
	require.Equal(t, credentialsecret.ReasonMalformed, step.reason)
	require.NotContains(t, step.message, "hunter2")
	require.NotContains(t, step.message, "api%.example.com")
}

// Plan F9: the names that coderd seeds from env (Coder v2.37.3).
func TestAIProviderNamesSeededByEnv(t *testing.T) {
	t.Parallel()
	fromSecret := &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "keys"}, Key: "key"}}
	for _, tc := range []struct {
		name       string
		env        []corev1.EnvVar
		want       map[string]string
		unresolved string
	}{
		{name: "legacy OpenAI key", env: []corev1.EnvVar{{Name: "CODER_AIBRIDGE_OPENAI_KEY", ValueFrom: fromSecret}}, want: map[string]string{"openai": "CODER_AIBRIDGE_OPENAI_KEY"}},
		{name: "legacy Anthropic key", env: []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_ANTHROPIC_KEY", Value: "sk-ant"}}, want: map[string]string{"anthropic": "CODER_AI_GATEWAY_ANTHROPIC_KEY"}},
		{name: "legacy Bedrock region", env: []corev1.EnvVar{{Name: "CODER_AIBRIDGE_BEDROCK_REGION", Value: "us-east-1"}}, want: map[string]string{"anthropic": "CODER_AIBRIDGE_BEDROCK_REGION"}},
		{name: "legacy Bedrock models only", env: []corev1.EnvVar{
			{Name: "CODER_AI_GATEWAY_BEDROCK_MODEL", Value: "claude"}, {Name: "CODER_AIBRIDGE_BEDROCK_SMALL_FAST_MODEL", Value: "haiku"},
		}, want: map[string]string{}},
		{name: "legacy base URL only", env: []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_OPENAI_BASE_URL", Value: "https://llm.example.com"}}, want: map[string]string{}},
		{name: "empty legacy key", env: []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_OPENAI_KEY"}}, want: map[string]string{}},
		{
			name: "indexed type without name", env: []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_PROVIDER_0_TYPE", Value: "openai"}, {Name: "CODER_AI_GATEWAY_PROVIDER_0_NAME"}},
			want: map[string]string{"openai": "CODER_AI_GATEWAY_PROVIDER_0_TYPE"},
		},
		{name: "indexed name", env: []corev1.EnvVar{
			{Name: "CODER_AIBRIDGE_PROVIDER_0_TYPE", Value: "openai"},
			{Name: "CODER_AIBRIDGE_PROVIDER_0_NAME", Value: "team-openai"},
			{Name: "CODER_AIBRIDGE_PROVIDER_1_NAME", Value: "first"},
			{Name: "CODER_AIBRIDGE_PROVIDER_1_NAME", Value: "last"},
			{Name: "CODER_AIBRIDGE_PROVIDER_1_KEY", Value: "sk"},
			{Name: "CODER_AIBRIDGE_PROVIDER_2_NAME", Value: "literal"},
			{Name: "CODER_AIBRIDGE_PROVIDER_2_TYPE", Value: "$(TYPE)"},
		}, want: map[string]string{"team-openai": "CODER_AIBRIDGE_PROVIDER_0_NAME", "last": "CODER_AIBRIDGE_PROVIDER_1_NAME", "literal": "CODER_AIBRIDGE_PROVIDER_2_NAME"}},
		{
			name: "indexed name from a Secret", env: []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_PROVIDER_0_TYPE", Value: "openai"}, {Name: "CODER_AI_GATEWAY_PROVIDER_0_NAME", ValueFrom: fromSecret}},
			want: map[string]string{}, unresolved: "CODER_AI_GATEWAY_PROVIDER_0_NAME",
		},
		{
			name: "indexed name with a reference", env: []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_PROVIDER_0_NAME", Value: "$(PROVIDER_NAME)"}},
			want: map[string]string{}, unresolved: "CODER_AI_GATEWAY_PROVIDER_0_NAME",
		},
		{
			name: "indexed type with a reference", env: []corev1.EnvVar{{Name: "CODER_AIBRIDGE_PROVIDER_0_TYPE", Value: "$(TYPE)"}},
			want: map[string]string{}, unresolved: "CODER_AIBRIDGE_PROVIDER_0_TYPE",
		},
		{name: "other variables", env: []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_PROVIDERS", Value: "openai"}, {Name: "CODER_ACCESS_URL", Value: "https://coder.example.com"}}, want: map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			names, unresolved := aiProviderNamesSeededByEnv(tc.env)
			require.Equal(t, tc.want, names)
			require.Equal(t, tc.unresolved, unresolved)
		})
	}
}
