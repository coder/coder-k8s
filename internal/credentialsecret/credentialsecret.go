// Package credentialsecret implements the credential Secret contract: the rules
// a Kubernetes Secret must meet before a controller sends its value to a remote
// service or puts it into a pod.
//
// A Secret is usable only when all of these are true:
//   - It has type Opaque.
//   - The operator did not generate it: it has no owner in the coder.com API
//     group, and it does not carry app.kubernetes.io/managed-by=coder-k8s.
//   - No CoderControlPlane in the namespace uses it for its own access: as its
//     operator token Secret, its license Secret, or its database Secret.
//   - It carries the label coder.com/credential=true.
//   - Its annotation coder.com/credential-destinations lists the origin of the
//     destination that the caller sends the value to.
//   - The key exists, and the value is non-empty, at most 8 KiB, and has no
//     leading or trailing whitespace.
//
// Errors from Read carry a condition reason and a message. Messages name
// Secrets, keys and origins only. They never hold Secret data.
package credentialsecret

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"net/url"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	// LabelKey is the opt-in label. Its value must be LabelValue.
	LabelKey = "coder.com/credential"
	// LabelValue is the only label value that opts a Secret in.
	LabelValue = "true"
	// DestinationsAnnotation lists the origins (scheme://host[:port]) that may
	// receive the Secret's values. Entries are separated by commas or whitespace.
	DestinationsAnnotation = "coder.com/credential-destinations"
	// MaxValueBytes is the largest value that Read accepts.
	MaxValueBytes = 8 * 1024

	managedByLabelKey   = "app.kubernetes.io/managed-by"
	managedByLabelValue = "coder-k8s"
	redacted            = "[REDACTED]"

	operatorTokenSecretSuffix = "-operator-token"
)

// Shared condition reasons for credential failures.
const (
	ReasonSecretNotFound        = "CredentialSecretNotFound"
	ReasonKeyMissing            = "CredentialKeyMissing"
	ReasonRefused               = "CredentialRefused"
	ReasonNotOptedIn            = "CredentialNotOptedIn"
	ReasonDestinationNotAllowed = "CredentialDestinationNotAllowed"
	ReasonMalformed             = "CredentialMalformed"
)

// Error is a contract failure. Reason is a condition reason. Message is safe to
// put into status, events and logs: it never holds Secret data.
type Error struct {
	Reason  string
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

func newError(reason, format string, args ...any) *Error {
	return &Error{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// ChangeToken identifies the Secret state that a Value came from. It holds no
// secret data.
type ChangeToken struct {
	SecretName      string
	SecretKey       string
	SecretUID       types.UID
	ResourceVersion string
}

// Value holds credential bytes. All formatting, logging and JSON encoding print
// "[REDACTED]". Use Reveal to get the bytes for the one call that needs them.
// Limit: fmt cannot call these methods on a Value inside an unexported struct
// field, and then prints the raw bytes. Keep a Value in a local variable or an
// exported field.
type Value struct {
	data  []byte
	Token ChangeToken
}

// Reveal returns the credential as a string.
func (v Value) Reveal() string {
	return string(v.data)
}

// String implements fmt.Stringer.
func (v Value) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (v Value) GoString() string { return redacted }

// Format implements fmt.Formatter, so every verb prints "[REDACTED]".
func (v Value) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redacted))
}

// MarshalJSON implements json.Marshaler.
func (v Value) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redacted + `"`), nil
}

// MarshalText implements encoding.TextMarshaler.
func (v Value) MarshalText() ([]byte, error) {
	return []byte(redacted), nil
}

// MarshalLog implements logr.Marshaler.
func (v Value) MarshalLog() any {
	return redacted
}

// Read gets the Secret ref.Name in namespace, applies the contract, and checks
// that the origin of destination is listed in the Secret's destinations.
// ref.Key must be set: callers apply their defaults first.
func Read(ctx context.Context, c client.Reader, namespace string, ref coderv1alpha1.SecretKeySelector, destination *url.URL) (Value, error) {
	if c == nil {
		return Value{}, fmt.Errorf("assertion failed: credential reader must not be nil")
	}
	if strings.TrimSpace(namespace) == "" {
		return Value{}, fmt.Errorf("assertion failed: credential namespace must not be empty")
	}
	if strings.TrimSpace(ref.Name) == "" {
		return Value{}, fmt.Errorf("assertion failed: credential Secret name must not be empty")
	}
	if strings.TrimSpace(ref.Key) == "" {
		return Value{}, fmt.Errorf("assertion failed: credential Secret key must not be empty for Secret %q", ref.Name)
	}
	if destination == nil {
		return Value{}, fmt.Errorf("assertion failed: credential destination must not be nil for Secret %q", ref.Name)
	}
	origin, err := Origin(destination)
	if err != nil {
		return Value{}, fmt.Errorf("assertion failed: credential destination for Secret %q: %w", ref.Name, err)
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return Value{}, newError(ReasonSecretNotFound, "Secret %q does not exist.", ref.Name)
		}
		return Value{}, fmt.Errorf("get credential Secret %q: %w", ref.Name, err)
	}
	if secret.Name != ref.Name || secret.Namespace != namespace {
		return Value{}, fmt.Errorf("assertion failed: fetched Secret %s/%s does not match %s/%s", secret.Namespace, secret.Name, namespace, ref.Name)
	}

	refused, err := isRefused(ctx, c, secret)
	if err != nil {
		return Value{}, err
	}
	if refused != nil {
		return Value{}, refused
	}
	if secret.Type != "" && secret.Type != corev1.SecretTypeOpaque {
		return Value{}, newError(ReasonRefused,
			"Secret %q has type %q. Only Opaque Secrets can be used as credentials.", ref.Name, string(secret.Type))
	}

	destinations, hasAnnotation := secret.Annotations[DestinationsAnnotation]
	if secret.Labels[LabelKey] != LabelValue || !hasAnnotation || strings.TrimSpace(destinations) == "" {
		return Value{}, newError(ReasonNotOptedIn,
			"Secret %q is not a consented credential. Add the label %s=%s and the annotation %s=%s.",
			ref.Name, LabelKey, LabelValue, DestinationsAnnotation, origin)
	}
	allowed, err := ParseDestinations(destinations)
	if err != nil {
		return Value{}, newError(ReasonMalformed, "Secret %q has an invalid annotation %s: %v.", ref.Name, DestinationsAnnotation, err)
	}
	if _, ok := allowed[origin]; !ok {
		return Value{}, newError(ReasonDestinationNotAllowed,
			"Secret %q does not allow %s. Add that origin to the annotation %s.", ref.Name, origin, DestinationsAnnotation)
	}

	data, ok := secret.Data[ref.Key]
	if !ok {
		return Value{}, newError(ReasonKeyMissing, "Secret %q has no key %q.", ref.Name, ref.Key)
	}
	switch {
	case len(data) == 0:
		return Value{}, newError(ReasonMalformed, "Secret %q key %q is empty.", ref.Name, ref.Key)
	case len(data) > MaxValueBytes:
		return Value{}, newError(ReasonMalformed, "Secret %q key %q is larger than %d bytes.", ref.Name, ref.Key, MaxValueBytes)
	case len(bytes.TrimSpace(data)) != len(data):
		return Value{}, newError(ReasonMalformed, "Secret %q key %q has leading or trailing whitespace.", ref.Name, ref.Key)
	}

	return Value{
		data: bytes.Clone(data),
		Token: ChangeToken{
			SecretName:      secret.Name,
			SecretKey:       ref.Key,
			SecretUID:       secret.UID,
			ResourceVersion: secret.ResourceVersion,
		},
	}, nil
}

// isRefused returns a ReasonRefused error when the operator generates secret
// or a CoderControlPlane in its namespace uses it for its own access.
func isRefused(ctx context.Context, c client.Reader, secret *corev1.Secret) (*Error, error) {
	generated := newError(ReasonRefused,
		"Secret %q is managed by coder-k8s and cannot be used here. Create a dedicated credential and store it in its own Secret.", secret.Name)
	if secret.Labels[managedByLabelKey] == managedByLabelValue {
		return generated, nil
	}
	for _, owner := range secret.OwnerReferences {
		// An owner whose apiVersion does not parse is refused too (fail closed).
		if group, ok := apiGroup(owner.APIVersion); !ok || group == coderv1alpha1.GroupVersion.Group {
			return generated, nil
		}
	}

	controlPlanes := &coderv1alpha1.CoderControlPlaneList{}
	if err := c.List(ctx, controlPlanes, client.InNamespace(secret.Namespace)); err != nil {
		return nil, fmt.Errorf("list codercontrolplanes to check credential Secret %q: %w", secret.Name, err)
	}
	for i := range controlPlanes.Items {
		controlPlane := &controlPlanes.Items[i]
		for _, used := range controlPlaneSecrets(controlPlane) {
			if used.name != secret.Name {
				continue
			}
			if used.role == roleOperatorToken {
				return generated, nil
			}
			return newError(ReasonRefused,
				"Secret %q is the %s Secret of CoderControlPlane %q and cannot be used here. Create a dedicated credential and store it in its own Secret.",
				secret.Name, used.role, controlPlane.Name), nil
		}
	}

	return nil, nil
}

// Roles of the Secrets that a control plane uses for its own access.
const (
	roleOperatorToken = "operator token"
	roleLicense       = "license"
	roleDatabase      = "database"
)

type controlPlaneSecret struct {
	name string
	role string
}

// controlPlaneSecrets returns the Secrets that controlPlane uses for its own
// access. Empty names are left out.
func controlPlaneSecrets(controlPlane *coderv1alpha1.CoderControlPlane) []controlPlaneSecret {
	if controlPlane == nil {
		return nil
	}
	candidates := []controlPlaneSecret{{name: OperatorTokenSecretName(controlPlane), role: roleOperatorToken}}
	if ref := controlPlane.Status.OperatorTokenSecretRef; ref != nil {
		candidates = append(candidates, controlPlaneSecret{name: ref.Name, role: roleOperatorToken})
	}
	if ref := controlPlane.Spec.LicenseSecretRef; ref != nil {
		candidates = append(candidates, controlPlaneSecret{name: ref.Name, role: roleLicense})
	}
	if db := controlPlane.Spec.Database; db != nil {
		candidates = append(candidates, controlPlaneSecret{name: db.ConnectionSecretRef.Name, role: roleDatabase})
	}
	secrets := make([]controlPlaneSecret, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.name = strings.TrimSpace(candidate.name); candidate.name != "" {
			secrets = append(secrets, candidate)
		}
	}
	return secrets
}

// ControlPlaneSecretNames returns the names of the Secrets that Read refuses
// because of controlPlane: its operator token Secret (the configured or default
// name, and the name in status), its license Secret, and its database Secret.
// The names are sorted and unique. Controllers re-queue the resources that
// reference these names when a control plane changes.
func ControlPlaneSecretNames(controlPlane *coderv1alpha1.CoderControlPlane) []string {
	secrets := controlPlaneSecrets(controlPlane)
	names := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		names = append(names, secret.name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func apiGroup(apiVersion string) (string, bool) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return "", false
	}
	return gv.Group, true
}

// OperatorTokenSecretName returns the name of the Secret that holds the
// operator token of controlPlane. The CoderControlPlane controller uses it to
// name that Secret, and Read uses it to refuse that Secret.
func OperatorTokenSecretName(controlPlane *coderv1alpha1.CoderControlPlane) string {
	if controlPlane == nil {
		return ""
	}

	configuredSecretName := strings.TrimSpace(controlPlane.Spec.OperatorAccess.GeneratedTokenSecretName)
	if configuredSecretName != "" {
		return configuredSecretName
	}

	candidate := controlPlane.Name + operatorTokenSecretSuffix
	if len(candidate) <= 63 {
		return candidate
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(controlPlane.Name))
	hashSuffix := fmt.Sprintf("%08x", hasher.Sum32())
	available := 63 - len(operatorTokenSecretSuffix) - len(hashSuffix) - 1
	if available < 1 {
		available = 1
	}

	return fmt.Sprintf("%s-%s%s", controlPlane.Name[:available], hashSuffix, operatorTokenSecretSuffix)
}

// Origin returns the normalized origin of u: lowercase scheme and host, and the
// port only when it is not the default port of the scheme. Only http and https
// URLs have an origin here.
func Origin(u *url.URL) (string, error) {
	if u == nil {
		return "", fmt.Errorf("URL must not be nil")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("URL scheme must be http or https")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("URL must have a host")
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

// ParseURL parses raw as an http or https URL that has a host.
func ParseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	if _, err := Origin(parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// ParseDestinations parses the destinations annotation into a set of origins.
// Each entry must be an origin: no user info, path, query or fragment.
func ParseDestinations(raw string) (map[string]struct{}, error) {
	entries := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(entries) == 0 {
		return nil, fmt.Errorf("no origins listed")
	}

	origins := make(map[string]struct{}, len(entries))
	for i, entry := range entries {
		parsed, err := url.Parse(entry)
		if err != nil {
			return nil, fmt.Errorf("entry %d is not a URL", i+1)
		}
		if parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
			return nil, fmt.Errorf("entry %d must be an origin such as https://coder.example.com", i+1)
		}
		origin, err := Origin(parsed)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i+1, err)
		}
		origins[origin] = struct{}{}
	}
	return origins, nil
}

// Redact replaces each value, and its base64 and URL-encoded forms, in s with
// "[REDACTED]".
func Redact(s string, values ...Value) string {
	for _, value := range values {
		if len(value.data) == 0 {
			continue
		}
		for _, form := range EncodedForms(value.data) {
			s = strings.ReplaceAll(s, form, redacted)
		}
	}
	return s
}

// EncodedForms returns the encoded forms of data that Redact removes, longest
// first so that a longer form is replaced before a form it contains.
func EncodedForms(data []byte) []string {
	raw := string(data)
	forms := []string{
		base64.StdEncoding.EncodeToString(data),
		base64.URLEncoding.EncodeToString(data),
		base64.RawStdEncoding.EncodeToString(data),
		base64.RawURLEncoding.EncodeToString(data),
		url.QueryEscape(raw),
		url.PathEscape(raw),
		raw,
	}

	unique := make([]string, 0, len(forms))
	seen := make(map[string]struct{}, len(forms))
	for _, form := range forms {
		if form == "" {
			continue
		}
		if _, ok := seen[form]; ok {
			continue
		}
		seen[form] = struct{}{}
		unique = append(unique, form)
	}
	slices.SortStableFunc(unique, func(a, b string) int { return len(b) - len(a) })
	return unique
}
