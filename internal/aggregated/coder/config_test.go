package coder

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewSDKClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		config          Config
		wantErrContains string
		wantTimeout     time.Duration
	}{
		{
			name: "defaults timeout when omitted",
			config: Config{
				CoderURL:     mustParseURL(t, "https://coder.example.com"),
				SessionToken: "session-token",
			},
			wantTimeout: defaultRequestTimeout,
		},
		{
			name: "uses explicit timeout",
			config: Config{
				CoderURL:       mustParseURL(t, "https://coder.example.com"),
				SessionToken:   "session-token",
				RequestTimeout: 45 * time.Second,
			},
			wantTimeout: 45 * time.Second,
		},
		{
			name: "rejects nil coder URL",
			config: Config{
				SessionToken: "session-token",
			},
			wantErrContains: "assertion failed: coder URL must not be nil",
		},
		{
			name: "rejects empty session token",
			config: Config{
				CoderURL: mustParseURL(t, "https://coder.example.com"),
			},
			wantErrContains: "assertion failed: session token must not be empty",
		},
		{
			name: "rejects negative timeout",
			config: Config{
				CoderURL:       mustParseURL(t, "https://coder.example.com"),
				SessionToken:   "session-token",
				RequestTimeout: -1 * time.Second,
			},
			wantErrContains: "assertion failed: request timeout must not be negative",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			client, err := NewSDKClient(testCase.config)
			if testCase.wantErrContains != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", testCase.wantErrContains)
				}
				if !strings.Contains(err.Error(), testCase.wantErrContains) {
					t.Fatalf("expected error to contain %q, got %q", testCase.wantErrContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if client == nil {
				t.Fatal("expected non-nil client")
			}
			if client.HTTPClient == nil {
				t.Fatal("expected non-nil HTTP client")
			}
			if got, want := client.HTTPClient.Timeout, testCase.wantTimeout; got != want {
				t.Fatalf("expected timeout %s, got %s", want, got)
			}
			if got, want := client.SessionToken(), testCase.config.SessionToken; got != want {
				t.Fatalf("expected session token %q, got %q", want, got)
			}
			if got, want := client.URL.String(), testCase.config.CoderURL.String(); got != want {
				t.Fatalf("expected URL %q, got %q", want, got)
			}
		})
	}
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", rawURL, err)
	}
	if parsedURL == nil {
		t.Fatalf("parse URL %q returned nil URL", rawURL)
	}

	return parsedURL
}
