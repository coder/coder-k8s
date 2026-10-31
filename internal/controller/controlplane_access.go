package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/coder/coder/v2/codersdk"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder-k8s/internal/controlplaneurl"
	"github.com/coder/coder-k8s/internal/operatortoken"
)

// controlPlaneCoderTimeout bounds each Coder request made with the operator
// token of a referenced CoderControlPlane.
const controlPlaneCoderTimeout = 30 * time.Second

// controlPlaneWait tells a controller that it cannot call Coder yet, and why.
// Reason is ControlPlaneNotReady or OperatorAccessNotReady.
type controlPlaneWait struct {
	reason  string
	message string
}

func newControlPlaneWait(reason, format string, args ...any) *controlPlaneWait {
	return &controlPlaneWait{reason: reason, message: fmt.Sprintf(format, args...)}
}

// controlPlaneCoderClient reads the CoderControlPlane name in namespace and
// its operator token, and returns a Coder client with that token. When the
// control plane cannot serve Coder calls yet, it returns a wait and no client.
func controlPlaneCoderClient(ctx context.Context, reader client.Reader, namespace, name string) (*codersdk.Client, *coderv1alpha1.CoderControlPlane, *controlPlaneWait, error) {
	return controlPlaneCoderClientFor(ctx, reader, namespace, name, false)
}

// controlPlaneCoderClientFor is controlPlaneCoderClient. With allowDeleting
// it also returns a client for a control plane that is being deleted, while
// its operator access still works. Only cleanups use that (plan D12).
func controlPlaneCoderClientFor(ctx context.Context, reader client.Reader, namespace, name string, allowDeleting bool) (*codersdk.Client, *coderv1alpha1.CoderControlPlane, *controlPlaneWait, error) {
	if reader == nil {
		return nil, nil, nil, errors.New("assertion failed: control plane access needs a Kubernetes reader")
	}
	if namespace == "" || name == "" {
		return nil, nil, nil, fmt.Errorf("assertion failed: control plane reference %q/%q needs a namespace and a name", namespace, name)
	}
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := reader.Get(ctx, key, controlPlane); apierrors.IsNotFound(err) {
		return nil, nil, newControlPlaneWait("ControlPlaneNotReady", "CoderControlPlane %s does not exist.", key.Name), nil
	} else if err != nil {
		return nil, nil, nil, fmt.Errorf("get codercontrolplane %s: %w", key, err)
	}
	switch {
	case !controlPlane.DeletionTimestamp.IsZero() && !allowDeleting:
		return nil, nil, newControlPlaneWait("ControlPlaneNotReady", "CoderControlPlane %s is being deleted.", key.Name), nil
	case controlPlane.Status.URL == "":
		return nil, nil, newControlPlaneWait("ControlPlaneNotReady", "CoderControlPlane %s has no status.url yet.", key.Name), nil
	case !controlPlane.Status.OperatorAccessReady || controlPlane.Status.OperatorTokenSecretRef == nil:
		return nil, nil, newControlPlaneWait("OperatorAccessNotReady", "CoderControlPlane %s has no operator access yet.", key.Name), nil
	}
	token, err := operatortoken.Read(ctx, reader, controlPlane)
	if errors.Is(err, operatortoken.ErrUnavailable) {
		return nil, nil, newControlPlaneWait("OperatorAccessNotReady", "CoderControlPlane %s: %v.", key.Name, err), nil
	} else if err != nil {
		return nil, nil, nil, fmt.Errorf("read operator token of codercontrolplane %s: %w", key, err)
	}
	coderURL, err := controlPlaneCoderURL(controlPlane)
	if err != nil {
		return nil, nil, newControlPlaneWait("ControlPlaneNotReady", "CoderControlPlane %s has no usable Coder URL: %v", key.Name, err), nil
	}
	trust, err := controlplaneurl.OperatorTLS(ctx, reader, controlPlane)
	if errors.Is(err, controlplaneurl.ErrUnavailable) {
		return nil, nil, newControlPlaneWait("ControlPlaneNotReady", "CoderControlPlane %s: %v.", key.Name, err), nil
	} else if err != nil {
		return nil, nil, nil, fmt.Errorf("read the TLS trust of codercontrolplane %s: %w", key, err)
	}
	sdk, err := coder.NewSDKClient(coder.Config{CoderURL: coderURL, TLS: trust, SessionToken: token, RequestTimeout: controlPlaneCoderTimeout})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create Coder client for codercontrolplane %s: %w", key, err)
	}
	return sdk, controlPlane, nil, nil
}

// controlPlaneCoderURL returns the URL for Coder calls (see
// controlplaneurl.CoderAPIURL).
func controlPlaneCoderURL(controlPlane *coderv1alpha1.CoderControlPlane) (*url.URL, error) {
	return controlplaneurl.CoderAPIURL(controlPlane)
}
