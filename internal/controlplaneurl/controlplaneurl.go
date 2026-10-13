// Package controlplaneurl derives the URL that operator components use for
// Coder API calls to a CoderControlPlane.
package controlplaneurl

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// DefaultServicePort is the Service port of a CoderControlPlane when
// spec.service.port is not set. Coder serves plain HTTP on it.
const DefaultServicePort = int32(80)

// TLSEnabled reports whether the control plane serves TLS.
func TLSEnabled(controlPlane *coderv1alpha1.CoderControlPlane) bool {
	return controlPlane != nil && len(controlPlane.Spec.TLS.SecretNames) > 0
}

// HTTPServicePort returns the Service port that serves plain HTTP.
func HTTPServicePort(controlPlane *coderv1alpha1.CoderControlPlane) (int32, error) {
	if controlPlane == nil {
		return 0, errors.New("assertion failed: coder control plane must not be nil")
	}
	servicePort := controlPlane.Spec.Service.Port
	if servicePort == 0 {
		servicePort = DefaultServicePort
	}
	if TLSEnabled(controlPlane) && servicePort == 443 {
		return DefaultServicePort, nil
	}
	return servicePort, nil
}

// InternalHTTPURL returns the plain HTTP URL of the control plane Service, or
// "" when it cannot be derived.
func InternalHTTPURL(controlPlane *coderv1alpha1.CoderControlPlane) string {
	servicePort, err := HTTPServicePort(controlPlane)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", controlPlane.Name, controlPlane.Namespace, servicePort)
}

// CoderAPIURL returns the URL for Coder API calls with the operator token.
// It is status.url when that is HTTP. With TLS, status.url is HTTPS on the
// Service name, which certificates rarely cover, so it is the internal HTTP
// URL instead. The operator token then travels over plain HTTP inside the
// cluster.
func CoderAPIURL(controlPlane *coderv1alpha1.CoderControlPlane) (*url.URL, error) {
	if controlPlane == nil {
		return nil, errors.New("assertion failed: coder control plane must not be nil")
	}
	// The url.Parse error already names the URL; callers show it as it is.
	statusURL, err := url.Parse(strings.TrimSpace(controlPlane.Status.URL))
	if err != nil {
		return nil, err
	}
	if statusURL.Scheme != "https" {
		return statusURL, nil
	}
	internalURL := InternalHTTPURL(controlPlane)
	if internalURL == "" {
		return nil, errors.New("no internal HTTP URL")
	}
	return url.Parse(internalURL)
}
