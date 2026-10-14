package coder

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/coder/v2/codersdk"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
)

// coderTimeoutMessage is the 504 message for a Coder call that ran out of time. It leaves out
// the Coder URL.
const coderTimeoutMessage = "the Coder API did not answer in time"

// errorLog records the full Coder error, which names the Coder URL, for operators. Kubernetes
// clients get a fixed message instead.
var errorLog = ctrl.Log.WithName("coder-errors")

// logHiddenError writes the full error at debug level.
func logHiddenError(err error, resource schema.GroupResource, name string, statusCode int) {
	errorLog.V(1).Info("Coder error hidden from the Kubernetes client",
		"resource", resource.String(), "name", name, "status", statusCode, "error", err.Error())
}

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
		// A request that did not reach Coder or got no answer. The transport error names the
		// Coder URL, so the message is fixed.
		// A net.Error that wraps no *url.Error, for example a connection reset while the body is
		// read, names Coder's address and port.
		var urlErr *url.Error
		if errors.As(err, &urlErr) || errors.As(err, &netErr) {
			logHiddenError(err, resource, name, 0)
			return apierrors.NewInternalError(fmt.Errorf("the Coder API could not be reached for %s %q", resource.Resource, name))
		}
		return apierrors.NewInternalError(err)
	}

	statusCode := coderErr.StatusCode()
	message := coderErrorMessage(coderErr, statusCode)

	switch statusCode {
	case http.StatusNotFound:
		return apierrors.NewNotFound(resource, name)
	case http.StatusForbidden:
		// The SDK error names the Coder URL and Coder's own message, so the message is fixed.
		logHiddenError(err, resource, name, statusCode)
		return apierrors.NewForbidden(resource, name, errors.New("the Coder API denied the request"))
	case http.StatusConflict:
		if isAlreadyExistsConflict(coderErr) {
			return apierrors.NewAlreadyExists(resource, name)
		}
		// The SDK error names the Coder URL and adds Coder's detail. Keep only Coder's message.
		logHiddenError(err, resource, name, statusCode)
		return apierrors.NewConflict(resource, name, errors.New(message))
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

		// A 5xx (other than 504) or an unexpected status. The SDK error names the Coder URL,
		// so the message is fixed.
		logHiddenError(err, resource, name, statusCode)
		return apierrors.NewInternalError(fmt.Errorf("the Coder API failed the request for %s %q with status %d", resource.Resource, name, statusCode))
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

// coderErrorMessage returns Coder's message for a 4xx answer. Without one, it returns a fixed
// message: the SDK error text names the Coder URL.
func coderErrorMessage(coderErr *codersdk.Error, statusCode int) string {
	if coderErr == nil {
		panic("assertion failed: coder error must not be nil")
	}

	message := strings.TrimSpace(coderErr.Message)
	if message != "" {
		return message
	}

	return fmt.Sprintf("the Coder API rejected the request with status %d", statusCode)
}

func isAlreadyExistsConflict(err *codersdk.Error) bool {
	if err == nil {
		panic("assertion failed: coder error must not be nil")
	}

	message := strings.ToLower(err.Message)

	return strings.Contains(message, "already exists")
}
