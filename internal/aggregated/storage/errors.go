package storage

import (
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/storage"
	storageerrors "k8s.io/apiserver/pkg/storage/errors"
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

// checkDeletePreconditions compares DeleteOptions.Preconditions (UID, resourceVersion) with the object
// fetched for this request and returns the same Conflict the generic registry produces on a mismatch.
// A nil options/preconditions value performs no check; an explicitly supplied empty UID or
// resourceVersion is compared like any other value. The comparison is a snapshot check: the caller
// must mutate the backend by the fetched object's ID afterwards, and the Coder APIs offer no
// compare-and-swap, so a change landing between the fetch and the mutation is not detected.
func checkDeletePreconditions(options *metav1.DeleteOptions, current runtime.Object, resource schema.GroupResource, name string) error {
	if options == nil || options.Preconditions == nil {
		return nil
	}
	if current == nil {
		return fmt.Errorf("assertion failed: fetched object for %s %q must not be nil", resource.Resource, name)
	}

	preconditions := storage.Preconditions{UID: options.Preconditions.UID, ResourceVersion: options.Preconditions.ResourceVersion}
	if err := preconditions.Check(name, current); err != nil {
		return storageerrors.InterpretDeleteError(err, resource, name)
	}

	return nil
}
