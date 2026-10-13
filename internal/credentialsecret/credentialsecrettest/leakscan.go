// Package credentialsecrettest holds test helpers for the credential Secret
// contract.
package credentialsecrettest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// RequireNoLeak fails t when any haystack holds a secret value, or one of the
// encoded forms that credentialsecret.Redact removes. A haystack is a string,
// an error, a fmt.Stringer, or any value that encodes to JSON (for example a
// status struct or an annotation map). name labels haystacks in failures.
func RequireNoLeak(t testing.TB, secrets []string, haystacks map[string]any) {
	t.Helper()
	if len(secrets) == 0 {
		t.Fatalf("assertion failed: RequireNoLeak needs at least one secret value")
	}

	for name, haystack := range haystacks {
		text := haystackText(t, name, haystack)
		for _, secret := range secrets {
			if secret == "" {
				t.Fatalf("assertion failed: RequireNoLeak secret values must not be empty")
			}
			for _, form := range credentialsecret.EncodedForms([]byte(secret)) {
				if strings.Contains(text, form) {
					t.Fatalf("%s holds a secret value (or an encoded form of it)", name)
				}
			}
		}
	}
}

func haystackText(t testing.TB, name string, haystack any) string {
	t.Helper()
	switch v := haystack.(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	case error:
		return v.Error()
	case fmt.Stringer:
		return v.String()
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("assertion failed: encode haystack %s: %v", name, err)
		}
		return string(encoded)
	}
}
