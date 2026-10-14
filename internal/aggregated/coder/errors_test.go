package coder

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestMapCoderError(t *testing.T) {
	t.Parallel()

	resource := aggregationv1alpha1.Resource("coderworkspaces")
	name := "acme.alice.dev"

	tests := []struct {
		name          string
		err           error
		assertMapping func(t *testing.T, err error)
	}{
		{
			name: "maps a timed-out Coder call to a gateway timeout",
			err:  fmt.Errorf("get workspace: %w", &url.Error{Op: "Get", URL: "https://coder.example.com", Err: context.DeadlineExceeded}),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsTimeout(err) || strings.Contains(err.Error(), "coder.example.com") {
					t.Fatalf("expected a Timeout without the URL, got %v", err)
				}
			},
		},
		{
			name: "maps a 504 from Coder to a gateway timeout",
			err:  codersdk.NewTestError(http.StatusGatewayTimeout, http.MethodGet, "https://coder.example.com"),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsTimeout(err) || strings.Contains(err.Error(), "coder.example.com") {
					t.Fatalf("expected a Timeout without the URL, got %v", err)
				}
			},
		},
		{
			name: "maps not found",
			err:  codersdk.NewTestError(http.StatusNotFound, http.MethodGet, "https://coder.example.com"),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsNotFound(err) {
					t.Fatalf("expected NotFound, got %v", err)
				}
			},
		},
		{
			name: "maps forbidden",
			err:  codersdk.NewTestError(http.StatusForbidden, http.MethodGet, "https://coder.example.com"),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsForbidden(err) {
					t.Fatalf("expected Forbidden, got %v", err)
				}
			},
		},
		{
			name: "maps bad request",
			err: withCoderMessage(
				codersdk.NewTestError(http.StatusBadRequest, http.MethodGet, "https://coder.example.com"),
				"bad workspace request",
			),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsBadRequest(err) {
					t.Fatalf("expected BadRequest, got %v", err)
				}
			},
		},
		{
			name: "maps unprocessable entity to bad request",
			err: withCoderMessage(
				codersdk.NewTestError(http.StatusUnprocessableEntity, http.MethodGet, "https://coder.example.com"),
				"invalid workspace transition",
			),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsBadRequest(err) {
					t.Fatalf("expected BadRequest, got %v", err)
				}
			},
		},
		{
			name: "maps unauthorized",
			err: withCoderMessage(
				codersdk.NewTestError(http.StatusUnauthorized, http.MethodGet, "https://coder.example.com"),
				"invalid session token",
			),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsUnauthorized(err) {
					t.Fatalf("expected Unauthorized, got %v", err)
				}
			},
		},
		{
			name: "maps too many requests",
			err: withCoderMessage(
				codersdk.NewTestError(http.StatusTooManyRequests, http.MethodGet, "https://coder.example.com"),
				"rate limited",
			),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsTooManyRequests(err) {
					t.Fatalf("expected TooManyRequests, got %v", err)
				}
				// The docs promise Coder's message and no Retry-After, so clients do not retry on
				// their own against the shared operator-token limit.
				if delay, ok := apierrors.SuggestsClientDelay(err); ok || delay != 0 || !strings.Contains(err.Error(), "rate limited") {
					t.Fatalf("expected Coder's message and no Retry-After, got %v (delay %d)", err, delay)
				}
			},
		},
		{
			name: "maps create conflict to already exists",
			err: withCoderMessage(
				codersdk.NewTestError(http.StatusConflict, http.MethodPost, "https://coder.example.com"),
				"workspace already exists",
			),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsAlreadyExists(err) {
					t.Fatalf("expected AlreadyExists, got %v", err)
				}
			},
		},
		{
			name: "maps update conflict to conflict",
			err: withCoderMessage(
				codersdk.NewTestError(http.StatusConflict, http.MethodPatch, "https://coder.example.com"),
				"resource version mismatch",
			),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsConflict(err) {
					t.Fatalf("expected Conflict, got %v", err)
				}
			},
		},
		{
			name: "maps coder internal errors",
			err:  codersdk.NewTestError(http.StatusInternalServerError, http.MethodGet, "https://coder.example.com"),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsInternalError(err) {
					t.Fatalf("expected InternalError, got %v", err)
				}
			},
		},
		{
			name: "maps generic errors to internal",
			err:  errors.New("boom"),
			assertMapping: func(t *testing.T, err error) {
				t.Helper()
				if !apierrors.IsInternalError(err) {
					t.Fatalf("expected InternalError, got %v", err)
				}
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			mappedErr := MapCoderError(testCase.err, resource, name)
			testCase.assertMapping(t, mappedErr)
		})
	}
}

func TestMapCoderErrorAssertions(t *testing.T) {
	t.Parallel()

	resource := aggregationv1alpha1.Resource("coderworkspaces")
	coderErr := codersdk.NewTestError(http.StatusNotFound, http.MethodGet, "https://coder.example.com")

	tests := []struct {
		name            string
		err             error
		resource        schema.GroupResource
		resourceName    string
		wantErrContains string
	}{
		{
			name:            "rejects nil error",
			err:             nil,
			resource:        resource,
			resourceName:    "acme.alice.dev",
			wantErrContains: "assertion failed: error must not be nil",
		},
		{
			name:            "rejects empty resource",
			err:             coderErr,
			resource:        schema.GroupResource{},
			resourceName:    "acme.alice.dev",
			wantErrContains: "assertion failed: resource must not be empty",
		},
		{
			name:            "rejects empty resource name",
			err:             coderErr,
			resource:        resource,
			resourceName:    "",
			wantErrContains: "assertion failed: resource name must not be empty",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := MapCoderError(testCase.err, testCase.resource, testCase.resourceName)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", testCase.wantErrContains)
			}
			if !strings.Contains(err.Error(), testCase.wantErrContains) {
				t.Fatalf("expected error containing %q, got %q", testCase.wantErrContains, err.Error())
			}
		})
	}
}

func withCoderMessage(err *codersdk.Error, message string) *codersdk.Error {
	if err == nil {
		panic("assertion failed: coder error must not be nil")
	}

	err.Message = message

	return err
}

func TestMapCoderCreateError(t *testing.T) {
	t.Parallel()

	resource := aggregationv1alpha1.Resource("codertemplates")
	name := "acme.docker"

	// Since codersdk v2.37, a 2xx whose body is not JSON (for example a proxy page) is a
	// *codersdk.Error with that status. Coder can have created the object.
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusNoContent} {
		err := MapCoderCreateError(fmt.Errorf("create: %w", codersdk.NewTestError(status, http.MethodPost, "https://coder.example.com")), resource, name)
		var statusErr *apierrors.StatusError
		if !errors.As(err, &statusErr) || !apierrors.IsTimeout(err) {
			t.Fatalf("status %d: expected a 504 Timeout, got %T %v", status, err, err)
		}
		message := statusErr.ErrStatus.Message
		for _, want := range []string{"uncertain", "re-read", `codertemplates "acme.docker"`, fmt.Sprint(status)} {
			if !strings.Contains(message, want) {
				t.Fatalf("status %d: message %q does not contain %q", status, message, want)
			}
		}
		if strings.Contains(message, "coder.example.com") {
			t.Fatalf("status %d: message %q must not contain the Coder URL", status, message)
		}
		if details := statusErr.ErrStatus.Details; details != nil && details.RetryAfterSeconds != 0 {
			t.Fatalf("status %d: an uncertain create must not ask clients to retry on their own, got Retry-After %d", status, details.RetryAfterSeconds)
		}
	}

	// Every other error maps as in MapCoderError.
	for _, err := range []error{
		codersdk.NewTestError(http.StatusConflict, http.MethodPost, "https://coder.example.com"),
		codersdk.NewTestError(http.StatusBadRequest, http.MethodPost, "https://coder.example.com"),
		codersdk.NewTestError(http.StatusBadGateway, http.MethodPost, "https://coder.example.com"),
		codersdk.NewTestError(http.StatusMultipleChoices, http.MethodPost, "https://coder.example.com"),
		errors.New("transport failed"),
	} {
		got, want := MapCoderCreateError(err, resource, name), MapCoderError(err, resource, name)
		if got.Error() != want.Error() || apierrors.ReasonForError(got) != apierrors.ReasonForError(want) {
			t.Fatalf("%v: expected the MapCoderError mapping %v, got %v", err, want, got)
		}
	}

	// The assertions run before the 2xx branch, so a 2xx error cannot hide them.
	ok := codersdk.NewTestError(http.StatusOK, http.MethodPost, "https://coder.example.com")
	for _, tc := range []struct {
		err      error
		resource schema.GroupResource
		name     string
		want     string
	}{
		{nil, resource, name, "assertion failed: error must not be nil"},
		{ok, schema.GroupResource{}, name, "assertion failed: resource must not be empty"},
		{ok, resource, "", "assertion failed: resource name must not be empty"},
	} {
		if err := MapCoderCreateError(tc.err, tc.resource, tc.name); err == nil || apierrors.IsTimeout(err) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("expected %q, got %v", tc.want, err)
		}
	}
}

// TestMapCoderErrorHidesCoderURL checks that a Coder 403, 5xx, unexpected status, transport
// error or 4xx without a message reaches the Kubernetes client without the Coder URL or Coder's
// own message, and keeps its status code.
func TestMapCoderErrorHidesCoderURL(t *testing.T) {
	t.Parallel()

	resource := aggregationv1alpha1.Resource("coderworkspaces")
	const (
		name       = "acme.alice.dev"
		coderURL   = "https://coder.internal.example.com/api/v2/workspaces/1234"
		coderHost  = "coder.internal.example.com"
		coderInner = "pq: relation workspaces_internal does not exist"
	)
	withMessage := func(status int) error {
		coderErr := codersdk.NewTestError(status, http.MethodGet, coderURL)
		coderErr.Message = coderInner
		coderErr.Detail = coderInner
		return fmt.Errorf("get workspace: %w", coderErr)
	}

	tests := []struct {
		name        string
		err         error
		wantCode    int32
		wantMessage string
	}{
		{name: "403", err: withMessage(http.StatusForbidden), wantCode: http.StatusForbidden, wantMessage: `coderworkspaces.aggregation.coder.com "acme.alice.dev" is forbidden: the Coder API denied the request`},
		{name: "500", err: withMessage(http.StatusInternalServerError), wantCode: http.StatusInternalServerError, wantMessage: `the Coder API failed the request for coderworkspaces "acme.alice.dev" with status 500`},
		{name: "502", err: withMessage(http.StatusBadGateway), wantCode: http.StatusInternalServerError, wantMessage: `the Coder API failed the request for coderworkspaces "acme.alice.dev" with status 502`},
		{name: "503", err: withMessage(http.StatusServiceUnavailable), wantCode: http.StatusInternalServerError, wantMessage: "with status 503"},
		{name: "unexpected 3xx", err: withMessage(http.StatusMultipleChoices), wantCode: http.StatusInternalServerError, wantMessage: "with status 300"},
		{
			name:        "transport error",
			err:         fmt.Errorf("get workspace: %w", &url.Error{Op: "Get", URL: coderURL, Err: errors.New("dial tcp 10.0.0.7:443: connect: connection refused")}),
			wantCode:    http.StatusInternalServerError,
			wantMessage: `the Coder API could not be reached for coderworkspaces "acme.alice.dev"`,
		},
		{
			// A 409 keeps Coder's message, which says what to do, but not the URL or the detail.
			name: "409 without already exists",
			err: func() error {
				coderErr := codersdk.NewTestError(http.StatusConflict, http.MethodPost, coderURL)
				coderErr.Message = "A workspace build is already active."
				coderErr.Detail = coderInner
				return fmt.Errorf("start workspace: %w", coderErr)
			}(),
			wantCode:    http.StatusConflict,
			wantMessage: `Operation cannot be fulfilled on coderworkspaces.aggregation.coder.com "acme.alice.dev": A workspace build is already active.`,
		},
		{
			name: "network error without a url.Error",
			err: fmt.Errorf("read workspace: %w", &net.OpError{
				Op: "read", Net: "tcp",
				Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.7"), Port: 443},
				Err:  errors.New("connection reset by peer"),
			}),
			wantCode:    http.StatusInternalServerError,
			wantMessage: `the Coder API could not be reached for coderworkspaces "acme.alice.dev"`,
		},
		{
			name:        "400 without a Coder message",
			err:         fmt.Errorf("get workspace: %w", codersdk.NewTestError(http.StatusBadRequest, http.MethodGet, coderURL)),
			wantCode:    http.StatusBadRequest,
			wantMessage: "the Coder API rejected the request with status 400",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(tt.err.Error(), coderHost) && !strings.Contains(tt.err.Error(), "10.0.0.7") {
				t.Fatalf("assertion failed: the test input must contain the Coder URL, got %q", tt.err.Error())
			}

			mapped := MapCoderError(tt.err, resource, name)
			var status apierrors.APIStatus
			if !errors.As(mapped, &status) {
				t.Fatalf("expected a Kubernetes API status error, got %T: %v", mapped, mapped)
			}
			if got := status.Status().Code; got != tt.wantCode {
				t.Fatalf("expected status code %d, got %d (%v)", tt.wantCode, got, mapped)
			}
			text := mapped.Error() + " " + status.Status().Message
			for _, leak := range []string{coderHost, "/api/v2", coderInner, "10.0.0.7"} {
				if strings.Contains(text, leak) {
					t.Fatalf("Kubernetes error contains %q: %q", leak, text)
				}
			}
			if !strings.Contains(mapped.Error(), tt.wantMessage) {
				t.Fatalf("expected message containing %q, got %q", tt.wantMessage, mapped.Error())
			}
		})
	}
}
