package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// Environment variables with which coderd seeds AI providers at startup
// (Coder v2.37.3 codersdk/deployment.go:1941-2021, :4455-4561 and
// cli/server.go:3174-3176). A seeded provider row with another config stops
// coderd from starting (coderd/ai_providers_migrate.go:76-146).
var (
	aiProviderLegacyEnv = map[string]string{
		"CODER_AI_GATEWAY_OPENAI_KEY":    "openai",
		"CODER_AIBRIDGE_OPENAI_KEY":      "openai",
		"CODER_AI_GATEWAY_ANTHROPIC_KEY": "anthropic",
		"CODER_AIBRIDGE_ANTHROPIC_KEY":   "anthropic",
	}
	// Any legacy Bedrock setting seeds the "anthropic" provider
	// (coderd/ai_providers_migrate.go:320-357).
	aiProviderBedrockEnvPrefixes = []string{"CODER_AI_GATEWAY_BEDROCK_", "CODER_AIBRIDGE_BEDROCK_"}
	// CODER_..._PROVIDER_<N>_NAME names provider N. Without a NAME, its TYPE
	// is the name (cli/server.go:3274-3276).
	aiProviderIndexedEnvPrefixes = []string{"CODER_AI_GATEWAY_PROVIDER_", "CODER_AIBRIDGE_PROVIDER_"}
)

// aiProviderNamesSeededByEnv maps each provider name that env seeds in coderd
// to the variable that seeds it. A legacy variable counts when it has a value
// or a valueFrom source. An indexed NAME or TYPE counts only with a literal
// value: the controller does not resolve valueFrom sources, and it does not
// inspect envFrom.
func aiProviderNamesSeededByEnv(env []corev1.EnvVar) map[string]string {
	names := map[string]string{}
	type indexed struct{ name, typ, nameVar, typeVar string }
	providers := map[string]*indexed{}
	var order []string
	for _, e := range env {
		if e.Value == "" && e.ValueFrom == nil {
			continue
		}
		if name, ok := aiProviderLegacyEnv[e.Name]; ok {
			names[name] = e.Name
		}
		for _, prefix := range aiProviderBedrockEnvPrefixes {
			if strings.HasPrefix(e.Name, prefix) {
				names["anthropic"] = e.Name
			}
		}
		for _, prefix := range aiProviderIndexedEnvPrefixes {
			index, field, ok := strings.Cut(strings.TrimPrefix(e.Name, prefix), "_")
			if !strings.HasPrefix(e.Name, prefix) || !ok || (field != "NAME" && field != "TYPE") {
				continue
			}
			key := prefix + index
			p := providers[key]
			if p == nil {
				p = &indexed{}
				providers[key] = p
				order = append(order, key)
			}
			// Kubernetes keeps the last of two variables with one name.
			if field == "NAME" {
				p.name, p.nameVar = e.Value, e.Name
			} else {
				p.typ, p.typeVar = e.Value, e.Name
			}
		}
	}
	for _, key := range order {
		switch p := providers[key]; {
		case p.name != "":
			names[p.name] = p.nameVar
		case p.typ != "":
			names[p.typ] = p.typeVar
		}
	}
	return names
}

// aiProviderReservedName returns a stall when the control plane's
// spec.extraEnv seeds a provider with the name of this object (F9).
func aiProviderReservedName(provider *coderv1alpha1.CoderAIProvider, controlPlane *coderv1alpha1.CoderControlPlane) *aiProviderStep {
	variable, ok := aiProviderNamesSeededByEnv(controlPlane.Spec.ExtraEnv)[provider.Name]
	if !ok {
		return nil
	}
	return aiProviderStalled(0, "NameReservedByEnv",
		"spec.extraEnv of CoderControlPlane %s sets %s, so coderd seeds a provider named %s at startup. The controller does not create a provider with that name, because coderd then fails to start. Choose another name.",
		controlPlane.Name, variable, provider.Name)
}
