package coder

import (
	"errors"
	"net/http"
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
