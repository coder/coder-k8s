package storage

import (
	"context"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
)

func resolveWriteNamespace(ctx context.Context, objectNamespace string) (string, error) {
	requestNamespace := genericapirequest.NamespaceValue(ctx)
	if requestNamespace == "" && objectNamespace == "" {
		return "", apierrors.NewBadRequest("namespace is required")
	}
	if requestNamespace == "" {
		return objectNamespace, nil
	}
	if objectNamespace == "" {
		return requestNamespace, nil
	}
	if requestNamespace != objectNamespace {
		return "", apierrors.NewBadRequest(fmt.Sprintf("request namespace %q does not match object namespace %q", requestNamespace, objectNamespace))
	}
	return requestNamespace, nil
}

func incrementResourceVersion(resourceVersion string) (string, error) {
	if resourceVersion == "" {
		return "1", nil
	}

	version, err := strconv.ParseInt(resourceVersion, 10, 64)
	if err != nil {
		return "", fmt.Errorf("assertion failed: invalid resourceVersion %q: %w", resourceVersion, err)
	}
	if version < 0 {
		return "", fmt.Errorf("assertion failed: resourceVersion must not be negative: %d", version)
	}

	return strconv.FormatInt(version+1, 10), nil
}
