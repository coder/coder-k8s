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
	// A legacy Bedrock base URL, region or access key seeds the "anthropic"
	// provider. The model variables have defaults, so they do not
	// (coderd/ai_providers_migrate.go:320-357, codersdk/aiproviders_bedrock.go:82-127).
	aiProviderBedrockEnv = map[string]bool{
		"CODER_AI_GATEWAY_BEDROCK_BASE_URL": true, "CODER_AI_GATEWAY_BEDROCK_REGION": true,
		"CODER_AI_GATEWAY_BEDROCK_ACCESS_KEY": true, "CODER_AI_GATEWAY_BEDROCK_ACCESS_KEY_SECRET": true,
		"CODER_AIBRIDGE_BEDROCK_BASE_URL": true, "CODER_AIBRIDGE_BEDROCK_REGION": true,
		"CODER_AIBRIDGE_BEDROCK_ACCESS_KEY": true, "CODER_AIBRIDGE_BEDROCK_ACCESS_KEY_SECRET": true,
	}
	// CODER_..._PROVIDER_<N>_NAME names provider N. Without a NAME, its TYPE
	// is the name (cli/server.go:3274-3276).
	aiProviderIndexedEnvPrefixes = []string{"CODER_AI_GATEWAY_PROVIDER_", "CODER_AIBRIDGE_PROVIDER_"}
)

// aiProviderNamesSeededByEnv maps each provider name that env seeds in coderd
// to the variable that seeds it. A legacy variable counts when it has a value
// or a valueFrom source. The controller does not resolve a valueFrom source
// or a $(VAR) reference, so an indexed name that needs one is returned as
// unresolved: then coderd can seed any name. envFrom and the seeding flags in
// spec.extraArgs are not inspected (#287).
func aiProviderNamesSeededByEnv(env []corev1.EnvVar) (names map[string]string, unresolved string) {
	names = map[string]string{}
	type field struct {
		value, variable string
		unknown         bool
	}
	type indexed struct{ name, typ field }
	providers := map[string]*indexed{}
	var order []string
	// Kubernetes keeps the last of two variables with one name, also an
	// empty one, so only the last entry counts. Coder uppercases each name
	// before it matches (serpent.ParseEnviron), so the check does too.
	last := map[string]corev1.EnvVar{}
	var variables []string
	for _, e := range env {
		e.Name = strings.ToUpper(e.Name)
		if _, ok := last[e.Name]; !ok {
			variables = append(variables, e.Name)
		}
		last[e.Name] = e
	}
	for _, variable := range variables {
		e := last[variable]
		if e.Value == "" && e.ValueFrom == nil {
			continue
		}
		if name, ok := aiProviderLegacyEnv[e.Name]; ok {
			names[name] = e.Name
		}
		if aiProviderBedrockEnv[e.Name] {
			names["anthropic"] = e.Name
		}
		for _, prefix := range aiProviderIndexedEnvPrefixes {
			index, key, ok := strings.Cut(strings.TrimPrefix(e.Name, prefix), "_")
			if !strings.HasPrefix(e.Name, prefix) || !ok || (key != "NAME" && key != "TYPE") {
				continue
			}
			p := providers[prefix+index]
			if p == nil {
				p = &indexed{}
				providers[prefix+index] = p
				order = append(order, prefix+index)
			}
			f := field{value: e.Value, variable: e.Name, unknown: e.ValueFrom != nil || strings.Contains(e.Value, "$(")}
			if key == "NAME" {
				p.name = f
			} else {
				p.typ = f
			}
		}
	}
	for _, key := range order {
		p := providers[key]
		f := p.name
		if f.variable == "" {
			f = p.typ
		}
		switch {
		case f.unknown:
			unresolved = f.variable
		case f.value != "":
			names[f.value] = f.variable
		}
	}
	return names, unresolved
}

// aiProviderReservedName returns a stall when the control plane's
// spec.extraEnv seeds a provider with the name of this object, or with a name
// that the controller cannot resolve (F9).
func aiProviderReservedName(provider *coderv1alpha1.CoderAIProvider, controlPlane *coderv1alpha1.CoderControlPlane) *aiProviderStep {
	names, unresolved := aiProviderNamesSeededByEnv(controlPlane.Spec.ExtraEnv)
	if variable, ok := names[provider.Name]; ok {
		return aiProviderStalled(0, "NameReservedByEnv",
			"spec.extraEnv of CoderControlPlane %s sets %s, so coderd seeds a provider named %s at startup. The controller does not create a provider with that name, because coderd then fails to start. Choose another name.",
			controlPlane.Name, variable, provider.Name)
	}
	if unresolved != "" {
		return aiProviderStalled(0, "NameReservedByEnv",
			"spec.extraEnv of CoderControlPlane %s sets %s from a valueFrom source or a $(VAR) reference, so the controller cannot tell which provider name coderd seeds. It creates no provider until that variable has a literal value.",
			controlPlane.Name, unresolved)
	}
	return nil
}
