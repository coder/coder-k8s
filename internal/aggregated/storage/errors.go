package storage

import (
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func wrapClientError(err error) error {
	if err == nil {
		return nil
	}

	var statusErr *apierrors.StatusError
	if errors.As(err, &statusErr) {
		return statusErr
	}

	return apierrors.NewInternalError(err)
}
