package coder

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/coder/coder/v2/codersdk"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// coderTimeoutMessage is the 504 message for a Coder call that ran out of time. It leaves out
// the Coder URL.
const coderTimeoutMessage = "the Coder API did not answer in time"

// MapCoderError converts Coder SDK errors to Kubernetes API errors.
func MapCoderError(err error, resource schema.GroupResource, name string) error {
	if err == nil {
		return fmt.Errorf("assertion failed: error must not be nil")
	}
	if resource.Empty() {
		return fmt.Errorf("assertion failed: resource must not be empty")
	}
	if name == "" {
		return fmt.Errorf("assertion failed: resource name must not be empty")
	}

	// A Coder call that ran out of time (its context deadline or the client's request timeout) is
	// a gateway timeout, not a server error. The message leaves out the Coder URL.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return apierrors.NewTimeoutError(coderTimeoutMessage, 0)
	}

	var coderErr *codersdk.Error
	if !errors.As(err, &coderErr) {
		return apierrors.NewInternalError(err)
	}

	statusCode := coderErr.StatusCode()
	message := coderErrorMessage(coderErr, err)

	switch statusCode {
	case http.StatusNotFound:
		return apierrors.NewNotFound(resource, name)
	case http.StatusForbidden:
		return apierrors.NewForbidden(resource, name, err)
	case http.StatusConflict:
		if isAlreadyExistsConflict(coderErr) {
			return apierrors.NewAlreadyExists(resource, name)
		}
		return apierrors.NewConflict(resource, name, err)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return apierrors.NewBadRequest(message)
	case http.StatusUnauthorized:
		return apierrors.NewUnauthorized(message)
	case http.StatusTooManyRequests:
		return apierrors.NewTooManyRequests(message, 0)
	case http.StatusGatewayTimeout:
		return apierrors.NewTimeoutError(coderTimeoutMessage, 0)
	default:
		if statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError {
			return apierrors.NewBadRequest(message)
		}

		return apierrors.NewInternalError(err)
	}
}

// MapCoderCreateError maps the error of the Coder POST that creates name. A 2xx *codersdk.Error
// means that the POST got through, but the answer is not the one that Coder sends: the SDK returns
// this error for a 2xx other than the expected one, and since codersdk v2.37 also for the expected
// 2xx with a body that is not JSON (for example a proxy page or an empty body). Coder can have
// created the object, so this is a 504 that says the result is uncertain, like any other write
// that times out. It has no Retry-After, so clients do not send the create again on their own.
// Every other error maps as in MapCoderError. That includes the SDK's plain errors for a body that
// breaks off while it is read and for malformed JSON on an unexpected 2xx (#242).
func MapCoderCreateError(err error, resource schema.GroupResource, name string) error {
	if err == nil {
		return fmt.Errorf("assertion failed: error must not be nil")
	}
	if resource.Empty() {
		return fmt.Errorf("assertion failed: resource must not be empty")
	}
	if name == "" {
		return fmt.Errorf("assertion failed: resource name must not be empty")
	}

	var coderErr *codersdk.Error
	if errors.As(err, &coderErr) && coderErr.StatusCode() >= http.StatusOK && coderErr.StatusCode() < http.StatusMultipleChoices {
		// The message leaves out the Coder URL.
		return apierrors.NewTimeoutError(fmt.Sprintf(
			"the result is uncertain: the Coder API answered the create of %s %q with status %d, but not with the answer that Coder sends; re-read the object with kubectl get before you retry",
			resource.Resource, name, coderErr.StatusCode(),
		), 0)
	}

	return MapCoderError(err, resource, name)
}

func coderErrorMessage(coderErr *codersdk.Error, fallback error) string {
	if coderErr == nil {
		panic("assertion failed: coder error must not be nil")
	}
	if fallback == nil {
		panic("assertion failed: fallback error must not be nil")
	}

	message := strings.TrimSpace(coderErr.Message)
	if message != "" {
		return message
	}

	return fallback.Error()
}

func isAlreadyExistsConflict(err *codersdk.Error) bool {
	if err == nil {
		panic("assertion failed: coder error must not be nil")
	}

	message := strings.ToLower(err.Message)

	return strings.Contains(message, "already exists")
}
