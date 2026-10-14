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

// Plan F9 and #296: the names that a control plane can seed in coderd. The
// rule fails closed: any value or none, any case.
func TestAIProviderSeededNames(t *testing.T) {
	t.Parallel()
	fromSecret := &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "keys"}, Key: "key"}}
	for _, tc := range []struct {
		name    string
		env     []corev1.EnvVar
		args    []string
		want    map[string]string
		blocker string
	}{
		{name: "legacy OpenAI key from a Secret", env: []corev1.EnvVar{{Name: "CODER_AIBRIDGE_OPENAI_KEY", ValueFrom: fromSecret}}, want: map[string]string{"openai": "spec.extraEnv sets CODER_AIBRIDGE_OPENAI_KEY"}},
		{name: "empty legacy key", env: []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_OPENAI_KEY"}}, want: map[string]string{"openai": "spec.extraEnv sets CODER_AI_GATEWAY_OPENAI_KEY"}},
		{name: "lowercase legacy key", env: []corev1.EnvVar{{Name: "coder_ai_gateway_anthropic_key", Value: "sk"}}, want: map[string]string{"anthropic": "spec.extraEnv sets coder_ai_gateway_anthropic_key"}},
		{name: "any Bedrock variable", env: []corev1.EnvVar{{Name: "CODER_AIBRIDGE_BEDROCK_MODEL", Value: "claude"}}, want: map[string]string{"anthropic": "spec.extraEnv sets CODER_AIBRIDGE_BEDROCK_MODEL"}},
		{name: "legacy base URL only", env: []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_OPENAI_BASE_URL", Value: "https://llm.example.com"}}, want: map[string]string{}},
		{
			name: "indexed variable", env: []corev1.EnvVar{{Name: "CODER_AIBRIDGE_PROVIDER_0_TYPE", Value: "openai"}, {Name: "coder_ai_gateway_provider_00_name"}},
			want: map[string]string{}, blocker: "spec.extraEnv sets CODER_AIBRIDGE_PROVIDER_0_TYPE",
		},
		{
			name: "flags", args: []string{"--ai-gateway-openai-key=sk-1", "-AIBRIDGE-BEDROCK-REGION", "us-east-1"},
			want: map[string]string{"openai": "spec.extraArgs passes --ai-gateway-openai-key", "anthropic": "spec.extraArgs passes --aibridge-bedrock-region"},
		},
		{name: "flag values", args: []string{"--oauth2-github-client-id", "ai-gateway-openai-key"}, want: map[string]string{}},
		{name: "argument reference", args: []string{"--log-human", "$(SEED_FLAG)"}, want: map[string]string{}, blocker: "spec.extraArgs[1] holds a $(VAR) reference"},
		{
			name: "config file path", env: []corev1.EnvVar{{Name: "coder_config_path", ValueFrom: fromSecret}},
			want: map[string]string{"anthropic": "spec.extraEnv sets coder_config_path, and a config file can hold Bedrock settings"},
		},
		{
			name: "config file flag", args: []string{"-c/etc/coder/secret-name.yaml"},
			want: map[string]string{"anthropic": "spec.extraArgs passes --config, and a config file can hold Bedrock settings"},
		},
		{name: "other settings", env: []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_PROVIDERS", Value: "openai"}}, args: []string{"--ai-gateway-openai-base-url=https://llm.example.com"}, want: map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			names, blocker := aiProviderSeededNames(&coderv1alpha1.CoderControlPlane{Spec: coderv1alpha1.CoderControlPlaneSpec{ExtraEnv: tc.env, ExtraArgs: tc.args}})
			require.Equal(t, tc.want, names)
			require.Equal(t, tc.blocker, blocker)
		})
	}
}

// #154 A7: every pflag form of --config and its shorthand -c passes a config
// file, which can seed anthropic. Other coder server shorthands take a value.
func TestAIProviderConfigFileFlagForms(t *testing.T) {
	t.Parallel()
	for arg, want := range map[string]bool{
		"--config": true, "--config=/etc/coder.yaml": true, "--CONFIG": true, "---config": true,
		"-c": true, "-c=/etc/coder.yaml": true, "-c/etc/coder.yaml": true, "-C": true, "-vc": true, "-vvc=/etc/coder.yaml": true,
		"-v": false, "-acoder.example.com": false, "-l0.0.0.0:3000": false, "--config-dir": false, "--cache-dir": false, "c": false, "config": false, "-": false,
	} {
		names, _ := aiProviderSeededNames(&coderv1alpha1.CoderControlPlane{Spec: coderv1alpha1.CoderControlPlaneSpec{ExtraArgs: []string{arg}}})
		_, got := names["anthropic"]
		require.Equal(t, want, got, "%q", arg)
	}
}

// The disable path refuses an object without a Coder provider ID: it has
// nothing to disable, and the caller must stall instead.
func TestDisableWithoutConsentNeedsProviderID(t *testing.T) {
	t.Parallel()
	r := &CoderAIProviderReconciler{}
	_, err := r.disableWithoutConsent(context.Background(), &coderv1alpha1.CoderAIProvider{}, &credentialsecret.Error{Reason: credentialsecret.ReasonSecretNotFound})
	require.ErrorContains(t, err, "assertion failed: AI provider")
}
