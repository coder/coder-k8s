package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

const (
	// reasonCredentialsAccepted is the CredentialsReady=True reason.
	reasonCredentialsAccepted  = "CredentialsAccepted"
	credentialsAcceptedMessage = "Every referenced credential Secret passed the credential checks."
	// reasonReservedSettingOverride means spec.extraEnv or spec.extraArgs sets a
	// value that the controller sets itself.
	reasonReservedSettingOverride = "ReservedSettingOverride"
	// reasonTokenSecretNotOwned means the generated proxy token Secret exists and
	// another owner (or no owner) controls it.
	reasonTokenSecretNotOwned = "TokenSecretNotOwned"
	// reasonKeySecretNotOwned means the provisioner key Secret exists and
	// another owner (or no owner) controls it.
	reasonKeySecretNotOwned = "KeySecretNotOwned"

	// credentialSecretNamesFieldIndex indexes CRs by the Secret names they
	// reference, so a Secret event re-queues only the CRs that use it.
	// #nosec G101 -- this is a field index key, not a credential.
	credentialSecretNamesFieldIndex = ".spec.credentialSecretNames"
)

// reservedSettings lists the env names and CLI flags that a controller sets
// itself. A CR must not override them through extraEnv or extraArgs.
type reservedSettings struct {
	envNames []string
	// flags are flag names without leading dashes.
	flags []string
}

// checkReservedSettings returns a ReservedSettingOverride error for the first
// extraEnv name or extraArgs flag that is reserved. The message names the
// entry but never its value.
//
// Env names match without case, because the Coder CLI folds env names to
// upper case. A flag matches in the forms --flag, --flag=value, -flag and
// -flag=value. Each arg is checked after the kubelet's $(VAR) expansion against
// the container env, so a variable cannot smuggle a reserved flag in.
func checkReservedSettings(extraEnv []corev1.EnvVar, extraArgs []string, reserved reservedSettings) error {
	for _, env := range extraEnv {
		for _, name := range reserved.envNames {
			if strings.EqualFold(env.Name, name) {
				return &credentialsecret.Error{
					Reason:  reasonReservedSettingOverride,
					Message: fmt.Sprintf("spec.extraEnv sets %s, which the operator sets itself. Remove the entry.", env.Name),
				}
			}
		}
	}

	known, unknown := containerEnvValues(reserved.envNames, extraEnv)
	for i, arg := range extraArgs {
		expanded, unknownRef := expandEnvRefs(arg, known, unknown, true)
		if unknownRef != "" {
			return &credentialsecret.Error{
				Reason: reasonReservedSettingOverride,
				Message: fmt.Sprintf("spec.extraArgs[%d] uses $(%s), whose value the operator cannot check for flags it sets itself. "+
					"Write the value in the arg, or escape the reference as $$(%s).", i, unknownRef, unknownRef),
			}
		}
		if flag, ok := reservedFlag(expanded, reserved.flags); ok {
			return &credentialsecret.Error{
				Reason:  reasonReservedSettingOverride,
				Message: fmt.Sprintf("spec.extraArgs passes --%s, which the operator sets itself. Remove the entry.", flag),
			}
		}
	}
	return nil
}

// containerEnvValues returns the env values that the kubelet uses to expand
// $(VAR) references in args: the operator-set variables first, then extraEnv
// in order, with each value expanded against the variables before it. The
// operator-set variables, variables set from valueFrom, and variables whose
// value references one of those are unknown.
func containerEnvValues(operatorEnv []string, extraEnv []corev1.EnvVar) (map[string]string, map[string]bool) {
	known := make(map[string]string, len(extraEnv))
	unknown := make(map[string]bool, len(operatorEnv))
	for _, name := range operatorEnv {
		unknown[name] = true
	}
	for _, env := range extraEnv {
		if env.ValueFrom != nil {
			delete(known, env.Name)
			unknown[env.Name] = true
			continue
		}
		value, unknownRef := expandEnvRefs(env.Value, known, unknown, false)
		if unknownRef != "" {
			delete(known, env.Name)
			unknown[env.Name] = true
			continue
		}
		delete(unknown, env.Name)
		known[env.Name] = value
	}
	return known, unknown
}

// expandEnvRefs expands s like the kubelet expands container args and env
// values: $$ becomes $, $(NAME) becomes the value of NAME when NAME is
// defined, and every other form stays as it is. It returns the first
// referenced name whose value is unknown.
//
// With arg set, an unknown reference counts only when it can still change the
// flag name: at the start, or while the text so far starts with "-" and has no
// "=". A later reference can only change a value, so expansion stops there and
// returns the text before it.
func expandEnvRefs(s string, known map[string]string, unknown map[string]bool, arg bool) (string, string) {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			out.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			out.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(s[i+2:], ')')
			if end < 0 {
				out.WriteString(s[i:])
				return out.String(), ""
			}
			name := s[i+2 : i+2+end]
			switch {
			case unknown[name]:
				prefix := out.String()
				if !arg || prefix == "" || (strings.HasPrefix(prefix, "-") && !strings.Contains(prefix, "=")) {
					return "", name
				}
				return prefix, ""
			case name != "":
				if value, ok := known[name]; ok {
					out.WriteString(value)
				} else {
					out.WriteString("$(" + name + ")")
				}
			default:
				out.WriteString("$()")
			}
			i += 2 + end
		default:
			out.WriteByte('$')
		}
	}
	return out.String(), ""
}

func reservedFlag(arg string, flags []string) (string, bool) {
	if !strings.HasPrefix(arg, "-") {
		return "", false
	}
	name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	name, _, _ = strings.Cut(name, "=")
	if slices.Contains(flags, name) {
		return name, true
	}
	return "", false
}

// checkExtraEnvSecrets applies the credential Secret contract to every
// extraEnv secretKeyRef. destination is the origin that the pod sends
// credentials to.
func checkExtraEnvSecrets(ctx context.Context, c client.Reader, namespace string, extraEnv []corev1.EnvVar, destination *url.URL) error {
	for i, env := range extraEnv {
		if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
			continue
		}
		field := fmt.Sprintf("spec.extraEnv[%d] (%s)", i, env.Name)
		ref := env.ValueFrom.SecretKeyRef
		if strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Key) == "" {
			return &credentialsecret.Error{
				Reason:  credentialsecret.ReasonMalformed,
				Message: fmt.Sprintf("%s: secretKeyRef must set both name and key.", field),
			}
		}
		if _, err := credentialsecret.Read(ctx, c, namespace, coderv1alpha1.SecretKeySelector{Name: ref.Name, Key: ref.Key}, destination); err != nil {
			return withCredentialField(err, field)
		}
	}
	return nil
}

// extraEnvSecretNames returns the Secret names that extraEnv references.
func extraEnvSecretNames(extraEnv []corev1.EnvVar) []string {
	names := make([]string, 0, len(extraEnv))
	for _, env := range extraEnv {
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name != "" {
			names = append(names, env.ValueFrom.SecretKeyRef.Name)
		}
	}
	return names
}

// withCredentialField prefixes a contract failure message with the spec field
// that referenced the Secret. Other errors pass through unchanged.
func withCredentialField(err error, field string) error {
	var credErr *credentialsecret.Error
	if !errors.As(err, &credErr) {
		return err
	}
	return &credentialsecret.Error{Reason: credErr.Reason, Message: field + ": " + credErr.Message}
}

// asCredentialError returns the contract failure in err, if any.
func asCredentialError(err error) (*credentialsecret.Error, bool) {
	var credErr *credentialsecret.Error
	if errors.As(err, &credErr) {
		return credErr, true
	}
	return nil, false
}

// sortedUnique returns the non-empty names in names, sorted and without
// duplicates.
func sortedUnique(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// isControlledBy reports whether the controller ownerReference of obj points to
// owner: a coder.com object of kind with owner's UID.
func isControlledBy(obj, owner metav1.Object, kind string) bool {
	ref := metav1.GetControllerOf(obj)
	if ref == nil || owner.GetUID() == "" {
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return false
	}
	return ref.UID == owner.GetUID() && gv.Group == coderv1alpha1.GroupVersion.Group && ref.Kind == kind
}
