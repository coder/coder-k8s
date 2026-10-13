package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
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
	case !controlPlane.DeletionTimestamp.IsZero():
		return nil, nil, newControlPlaneWait("ControlPlaneNotReady", "CoderControlPlane %s is being deleted.", key.Name), nil
	case controlPlane.Status.URL == "":
		return nil, nil, newControlPlaneWait("ControlPlaneNotReady", "CoderControlPlane %s has no status.url yet.", key.Name), nil
	case !controlPlane.Status.OperatorAccessReady || controlPlane.Status.OperatorTokenSecretRef == nil:
		return nil, nil, newControlPlaneWait("OperatorAccessNotReady", "CoderControlPlane %s has no operator access yet.", key.Name), nil
	}
	ref := controlPlane.Status.OperatorTokenSecretRef
	secretKey := strings.TrimSpace(ref.Key)
	if secretKey == "" {
		secretKey = coderv1alpha1.DefaultTokenSecretKey
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: strings.TrimSpace(ref.Name)}, secret); client.IgnoreNotFound(err) != nil {
		return nil, nil, nil, fmt.Errorf("read operator token of codercontrolplane %s: %w", key, err)
	}
	token := string(secret.Data[secretKey])
	if token == "" {
		return nil, nil, newControlPlaneWait("OperatorAccessNotReady", "The operator token Secret of CoderControlPlane %s has no token yet.", key.Name), nil
	}
	coderURL, err := controlPlaneCoderURL(controlPlane)
	if err != nil {
		return nil, nil, newControlPlaneWait("ControlPlaneNotReady", "CoderControlPlane %s has no usable Coder URL: %v", key.Name, err), nil
	}
	sdk, err := coder.NewSDKClient(coder.Config{CoderURL: coderURL, SessionToken: token, RequestTimeout: controlPlaneCoderTimeout})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create Coder client for codercontrolplane %s: %w", key, err)
	}
	return sdk, controlPlane, nil, nil
}

// controlPlaneCoderURL returns the URL for Coder calls. With TLS, status.url
// is HTTPS on the service name, which certificates rarely cover, so the
// controller uses the internal HTTP URL like the control-plane controller.
func controlPlaneCoderURL(controlPlane *coderv1alpha1.CoderControlPlane) (*url.URL, error) {
	statusURL, err := url.Parse(controlPlane.Status.URL)
	if err != nil || statusURL.Scheme != "https" {
		return statusURL, err
	}
	internalURL := controlPlaneSDKURL(controlPlane)
	if internalURL == "" {
		return nil, errors.New("no internal HTTP URL")
	}
	return url.Parse(internalURL)
}
