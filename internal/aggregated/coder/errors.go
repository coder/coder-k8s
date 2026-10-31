package coder

import (
	"crypto/tls"
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

	"github.com/coder/coder-k8s/internal/sameorigin"
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

// Write names the Coder write in the message of an uncertain result.
type Write string

// The writes of the aggregated storages.
const (
	WriteCreate Write = "create"
	WriteUpdate Write = "update"
	WriteDelete Write = "delete"
)

// IsUncertainStatus reports whether a Coder or proxy status leaves it open whether Coder applied
// a write. A 2xx status means that the request got through, but the answer is not the one that
// Coder sends: the SDK returns a *codersdk.Error for a 2xx other than the expected one, and since
// codersdk v2.37 also for the expected 2xx with a body that is not JSON (for example a proxy page
// or an empty body). The SDK sees a 3xx only when net/http did not follow it, for example a proxy
// redirect without Location, and the server that answered it got the write. A proxy in front of
// Coder can answer 502, 503 or 504 after Coder applied the write.
func IsUncertainStatus(status int) bool {
	return (status >= http.StatusOK && status < http.StatusBadRequest) ||
		status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// IsCoderErrorAnswer reports whether coderErr comes from an error answer in Coder's own format: a
// JSON body with a message. For a body that is not JSON, is empty, or has no message, for example
// the error page of a proxy, the SDK sets one of its own messages instead.
func IsCoderErrorAnswer(coderErr *codersdk.Error) bool {
	if coderErr == nil {
		panic("assertion failed: coder error must not be nil")
	}
	message := coderErr.Message
	return message != "" && message != "empty response body" &&
		!strings.HasPrefix(message, "unexpected non-JSON response ") &&
		message != fmt.Sprintf("unexpected status code %d, response has no message", coderErr.StatusCode())
}

// MapCoderWriteError maps the error of the Coder call that creates, updates or deletes name. An
// error that leaves it open whether Coder applied the write returns UncertainWriteError: a status
// of IsUncertainStatus other than 504, an error answer that is not Coder's own (see
// IsCoderErrorAnswer), or an error without a status after the request was sent,
// such as a connection that broke or a body that is not the expected JSON. A timeout, a Coder 504,
// a request that did not reach Coder (see notSent) and every other status map as in MapCoderError.
func MapCoderWriteError(err error, write Write, resource schema.GroupResource, name string) error {
	if err == nil {
		return fmt.Errorf("assertion failed: error must not be nil")
	}
	if write == "" {
		return fmt.Errorf("assertion failed: write must not be empty")
	}
	if resource.Empty() {
		return fmt.Errorf("assertion failed: resource must not be empty")
	}
	if name == "" {
		return fmt.Errorf("assertion failed: resource name must not be empty")
	}

	var coderErr *codersdk.Error
	if errors.As(err, &coderErr) {
		status := coderErr.StatusCode()
		if status != http.StatusGatewayTimeout && (IsUncertainStatus(status) || !IsCoderErrorAnswer(coderErr)) {
			logHiddenError(err, resource, name, status)
			return UncertainWriteError(write, resource, name, fmt.Sprintf("status %d from the Coder API", status))
		}
		return MapCoderError(err, resource, name)
	}

	var netErr net.Error
	if (errors.As(err, &netErr) && netErr.Timeout()) || notSent(err) {
		return MapCoderError(err, resource, name)
	}
	logHiddenError(err, resource, name, 0)
	return UncertainWriteError(write, resource, name, "an answer from the Coder API that broke off or could not be read")
}

// UncertainWriteErrorFrom is UncertainWriteError for a write whose result the Coder call that
// failed with err could not confirm. It logs err, which names the Coder URL, for operators.
func UncertainWriteErrorFrom(err error, write Write, resource schema.GroupResource, name, reason string) error {
	if err == nil {
		return fmt.Errorf("assertion failed: error must not be nil")
	}
	LogHiddenError(err, resource, name)
	return UncertainWriteError(write, resource, name, reason)
}

// LogHiddenError logs err, which can name the Coder URL, for operators, with the status of its
// *codersdk.Error or 0 for an error without a status.
func LogHiddenError(err error, resource schema.GroupResource, name string) {
	status := 0
	var coderErr *codersdk.Error
	if errors.As(err, &coderErr) {
		status = coderErr.StatusCode()
	}
	logHiddenError(err, resource, name, status)
}

// UncertainWriteError is the 504 for a write that Coder can have applied. reason names the answer
// that the write got, for example "status 502 from the Coder API", and must not name the Coder
// URL. The error has no Retry-After, so clients do not send the write again on their own.
func UncertainWriteError(write Write, resource schema.GroupResource, name, reason string) error {
	return apierrors.NewTimeoutError(fmt.Sprintf(
		"the result is uncertain: the %s of %s %q got %s; re-read the object with kubectl get before you retry",
		write, resource.Resource, name, reason,
	), 0)
}

// notSent reports whether err proves that the request did not reach Coder. Each error that it
// accepts comes from a step before net/http writes the request: the dial of the connection to Coder
// or to the proxy in front of it, the proxy answer to the CONNECT tunnel, the verification of the
// certificate, a first record of the server that is not TLS (crypto/tls sets Conn only before the
// server hello), and the refusal of a request to another origin. A received TLS alert is not in
// this list: crypto/tls returns an alert that the server sends after it read the request in the
// same way as one that refuses the handshake. A refused redirect is not in this list either, and
// neither is an error of a request that followed a redirect: the server that answered with the
// redirect got the write. An error after net/http wrote the request (errAfterWrite) never proves
// it, even when a retry on a new connection failed in one of those steps.
func notSent(err error) bool {
	if errors.Is(err, errAfterRedirect) || errors.Is(err, errAfterWrite) {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && (opErr.Op == "dial" || opErr.Op == "proxyconnect") {
		return true
	}
	var certErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	return errors.As(err, &certErr) || (errors.As(err, &recordErr) && recordErr.Conn != nil) ||
		errors.Is(err, http.ErrSchemeMismatch) || errors.Is(err, errProxyConnectRefused) ||
		errors.Is(err, sameorigin.ErrCrossOriginRequest)
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
