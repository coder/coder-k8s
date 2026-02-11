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

	switch coderErr.StatusCode() {
	case http.StatusNotFound:
		return apierrors.NewNotFound(resource, name)
	case http.StatusForbidden:
		return apierrors.NewForbidden(resource, name, err)
	case http.StatusConflict:
		if isAlreadyExistsConflict(coderErr) {
			return apierrors.NewAlreadyExists(resource, name)
		}
		return apierrors.NewConflict(resource, name, err)
	default:
		return apierrors.NewInternalError(err)
	}
}

func isAlreadyExistsConflict(err *codersdk.Error) bool {
	if err == nil {
		panic("assertion failed: coder error must not be nil")
	}

	message := strings.ToLower(err.Message)

	return strings.Contains(message, "already exists")
}
