package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
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
