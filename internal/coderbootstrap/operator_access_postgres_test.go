package coderbootstrap

import (
	"context"
	"regexp"
	"testing"
	"time"
)

func TestEnsureOperatorTokenRequestValidate(t *testing.T) {
	t.Parallel()

	req := EnsureOperatorTokenRequest{}
	if err := req.validate(); err == nil {
		t.Fatal("expected validate to fail for empty request")
	}

	req = EnsureOperatorTokenRequest{
		PostgresURL:      "postgres://example.com/coder",
		OperatorUsername: "coder-k8s-operator",
		OperatorEmail:    "coder-k8s-operator@coder-k8s.invalid",
		TokenName:        "coder-k8s-operator",
		TokenLifetime:    time.Hour,
	}
	if err := req.validate(); err != nil {
		t.Fatalf("expected validate to pass for complete request, got %v", err)
	}
}

func TestRevokeOperatorTokenRequestValidate(t *testing.T) {
	t.Parallel()

	req := RevokeOperatorTokenRequest{}
	if err := req.validate(); err == nil {
		t.Fatal("expected validate to fail for empty request")
	}

	req = RevokeOperatorTokenRequest{
		PostgresURL:      "postgres://example.com/coder",
		OperatorUsername: "coder-k8s-operator",
		TokenName:        "coder-k8s-operator",
	}
	if err := req.validate(); err != nil {
		t.Fatalf("expected validate to pass for complete revoke request, got %v", err)
	}
}

func TestRandomTokenPart_GeneratesExpectedLengthAndCharset(t *testing.T) {
	t.Parallel()

	const tokenLength = 64
	tokenPart, err := randomTokenPart(tokenLength)
	if err != nil {
		t.Fatalf("randomTokenPart returned unexpected error: %v", err)
	}
	if len(tokenPart) != tokenLength {
		t.Fatalf("expected token part length %d, got %d", tokenLength, len(tokenPart))
	}

	validCharset := regexp.MustCompile("^[0-9A-Za-z]+$")
	if !validCharset.MatchString(tokenPart) {
		t.Fatalf("expected token part to match charset [0-9A-Za-z], got %q", tokenPart)
	}
}

func TestSplitOperatorToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		input          string
		expectedID     string
		expectedSecret string
		expectedOK     bool
	}{
		{
			name:           "valid token",
			input:          "abcdefghij-1234567890123456789012",
			expectedID:     "abcdefghij",
			expectedSecret: "1234567890123456789012",
			expectedOK:     true,
		},
		{name: "missing separator", input: "abcdefghij123", expectedOK: false},
		{name: "missing id", input: "-secret", expectedOK: false},
		{name: "missing secret", input: "id-", expectedOK: false},
		{name: "empty token", input: "", expectedOK: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotID, gotSecret, gotOK := splitOperatorToken(tc.input)
			if gotOK != tc.expectedOK {
				t.Fatalf("expected ok=%t, got %t", tc.expectedOK, gotOK)
			}
			if gotID != tc.expectedID {
				t.Fatalf("expected id %q, got %q", tc.expectedID, gotID)
			}
			if gotSecret != tc.expectedSecret {
				t.Fatalf("expected secret %q, got %q", tc.expectedSecret, gotSecret)
			}
		})
	}
}

func TestRevokeOperatorToken_ValidatesInputsBeforeConnecting(t *testing.T) {
	t.Parallel()

	provisioner := NewPostgresOperatorAccessProvisioner()
	if provisioner == nil {
		t.Fatal("expected non-nil provisioner")
	}

	err := provisioner.RevokeOperatorToken(context.Background(), RevokeOperatorTokenRequest{})
	if err == nil {
		t.Fatal("expected RevokeOperatorToken to fail for invalid request")
	}
}

func TestEnsureOperatorToken_ValidatesInputsBeforeConnecting(t *testing.T) {
	t.Parallel()

	provisioner := NewPostgresOperatorAccessProvisioner()
	if provisioner == nil {
		t.Fatal("expected non-nil provisioner")
	}

	_, err := provisioner.EnsureOperatorToken(context.Background(), EnsureOperatorTokenRequest{})
	if err == nil {
		t.Fatal("expected EnsureOperatorToken to fail for invalid request")
	}
}
