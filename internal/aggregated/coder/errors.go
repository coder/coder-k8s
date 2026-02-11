package coder

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coder/coder/v2/codersdk"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

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
	default:
		if statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError {
			return apierrors.NewBadRequest(message)
		}

		return apierrors.NewInternalError(err)
	}
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
