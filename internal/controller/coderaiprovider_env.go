package controller

import (
	"strings"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// coderd seeds AI providers from env and flags at startup (Coder v2.37.3
// codersdk/deployment.go:1929-2021, :4440-4561 and cli/server.go:3174-3176).
// A seeded provider row with another config stops coderd from starting
// (coderd/ai_providers_migrate.go:76-146).
//
// The check fails closed instead of resolving values (#296): Coder
// uppercases variable names, applies alias and duplicate rules, and the
// kubelet expands references, so a resolved value can differ from what
// coderd sees.
var (
	// A legacy key or Bedrock setting seeds the provider of its name, with
	// any value or none.
	aiProviderLegacyEnv = map[string]string{
		"CODER_AI_GATEWAY_OPENAI_KEY":    "openai",
		"CODER_AIBRIDGE_OPENAI_KEY":      "openai",
		"CODER_AI_GATEWAY_ANTHROPIC_KEY": "anthropic",
		"CODER_AIBRIDGE_ANTHROPIC_KEY":   "anthropic",
	}
	aiProviderBedrockEnvPrefixes = []string{"CODER_AI_GATEWAY_BEDROCK_", "CODER_AIBRIDGE_BEDROCK_"}
	// The same settings as flags (#287).
	aiProviderLegacyFlags = map[string]string{
		"ai-gateway-openai-key":    "openai",
		"aibridge-openai-key":      "openai",
		"ai-gateway-anthropic-key": "anthropic",
		"aibridge-anthropic-key":   "anthropic",
	}
	aiProviderBedrockFlagPrefixes = []string{"ai-gateway-bedrock-", "aibridge-bedrock-"}
	// CODER_..._PROVIDER_<N>_* configures an indexed provider, whose name
	// any of its variables can set. These have no flags.
	aiProviderIndexedEnvPrefixes = []string{"CODER_AI_GATEWAY_PROVIDER_", "CODER_AIBRIDGE_PROVIDER_"}
)

// aiProviderSeededNames maps each provider name that the control plane can
// seed in coderd to the setting that seeds it. blocker names a setting that
// can seed any name. envFrom sources and config files are not inspected.
func aiProviderSeededNames(controlPlane *coderv1alpha1.CoderControlPlane) (names map[string]string, blocker string) {
	names = map[string]string{}
	for _, e := range controlPlane.Spec.ExtraEnv {
		name := strings.ToUpper(e.Name)
		if seeded, ok := aiProviderLegacyEnv[name]; ok {
			names[seeded] = "spec.extraEnv sets " + e.Name
		}
		for _, prefix := range aiProviderBedrockEnvPrefixes {
			if strings.HasPrefix(name, prefix) {
				names["anthropic"] = "spec.extraEnv sets " + e.Name
			}
		}
		for _, prefix := range aiProviderIndexedEnvPrefixes {
			if strings.HasPrefix(name, prefix) && blocker == "" {
				blocker = "spec.extraEnv sets " + e.Name
			}
		}
	}
	// Only the flag name goes into a message: an argument can hold a key.
	for _, arg := range controlPlane.Spec.ExtraArgs {
		flag, _, _ := strings.Cut(strings.ToLower(strings.TrimLeft(arg, "-")), "=")
		if seeded, ok := aiProviderLegacyFlags[flag]; ok {
			names[seeded] = "spec.extraArgs passes --" + flag
		}
		for _, prefix := range aiProviderBedrockFlagPrefixes {
			if strings.HasPrefix(flag, prefix) {
				names["anthropic"] = "spec.extraArgs passes --" + flag
			}
		}
	}
	return names, blocker
}

// aiProviderReservedName returns a stall when the control plane can seed a
// provider with the name of this object (F9). It runs before a create only.
// A seeding setting added after the controller created its provider is not
// guarded: the control plane rolls coderd at once, before this controller can
// act, and coderd then refuses to start if the configs differ. Remove the
// setting, or rename the CoderAIProvider.
func aiProviderReservedName(provider *coderv1alpha1.CoderAIProvider, controlPlane *coderv1alpha1.CoderControlPlane) *aiProviderStep {
	names, blocker := aiProviderSeededNames(controlPlane)
	if source, ok := names[provider.Name]; ok {
		return aiProviderStalled(0, "NameReservedByEnv",
			"CoderControlPlane %s %s, so coderd can seed a provider named %s at startup. The controller does not create a provider with that name, because coderd then fails to start. Choose another name.",
			controlPlane.Name, source, provider.Name)
	}
	if blocker != "" {
		return aiProviderStalled(0, "NameReservedByEnv",
			"CoderControlPlane %s %s, an indexed AI provider setting that can seed any provider name. The controller creates no provider while it is set.",
			controlPlane.Name, blocker)
	}
	return nil
}
