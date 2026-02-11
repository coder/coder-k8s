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
