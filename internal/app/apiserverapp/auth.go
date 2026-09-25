package apiserverapp

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/client-go/tools/clientcmd"
)

// anonymousAllowedPaths are the only paths an unauthenticated caller may reach. They are exact
// matches: kubelet probes use them, and they return no Coder data.
var anonymousAllowedPaths = map[string]struct{}{
	"/healthz": {},
	"/livez":   {},
	"/readyz":  {},
}

// anonymousHealthOnly refuses the anonymous identity on every path except anonymousAllowedPaths.
//
// The vendored DelegatingAuthenticationOptions.ApplyTo always appends an anonymous fallback
// (k8s.io/apiserver/pkg/server/options/authentication.go), so without this wrapper "no data for
// anonymous callers" would depend on cluster RBAC never granting system:anonymous or
// system:unauthenticated anything on aggregation.coder.com.
type anonymousHealthOnly struct {
	delegate authenticator.Request
}

var _ authenticator.Request = anonymousHealthOnly{}

func (a anonymousHealthOnly) AuthenticateRequest(req *http.Request) (*authenticator.Response, bool, error) {
	if a.delegate == nil {
		return nil, false, fmt.Errorf("assertion failed: delegate authenticator must not be nil")
	}
	resp, ok, err := a.delegate.AuthenticateRequest(req)
	// Errors and "not authenticated" pass through unchanged; both end in 401.
	if err != nil || !ok {
		return resp, ok, err
	}
	if resp == nil || resp.User == nil {
		return nil, false, fmt.Errorf("assertion failed: authenticated response must carry a user")
	}
	if resp.User.GetName() != user.Anonymous {
		return resp, ok, nil
	}
	if _, allowed := anonymousAllowedPaths[req.URL.Path]; allowed {
		return resp, ok, nil
	}
	return nil, false, nil
}

// newDelegatedAuthOptions returns the production delegated authentication and authorization
// options. Both use the same Kubernetes API authority, resolved by resolveDelegationKubeconfig.
// Every other setting is the vendored default: lookup failures other than NotFound are fatal,
// system:masters bypasses SubjectAccessReview, and decisions are cached (see docs).
func newDelegatedAuthOptions(kubeconfigPath string) (*genericoptions.DelegatingAuthenticationOptions, *genericoptions.DelegatingAuthorizationOptions) {
	authn := genericoptions.NewDelegatingAuthenticationOptions()
	authn.RemoteKubeConfigFile = kubeconfigPath
	authn.RemoteKubeConfigFileOptional = false
	authn.SkipInClusterLookup = false
	authn.TolerateInClusterLookupFailure = false

	authz := genericoptions.NewDelegatingAuthorizationOptions()
	authz.RemoteKubeConfigFile = kubeconfigPath
	authz.RemoteKubeConfigFileOptional = false

	return authn, authz
}

// resolveDelegationKubeconfig picks the Kubernetes API authority used for TokenReview,
// SubjectAccessReview, and the extension-apiserver-authentication ConfigMap. It follows the same
// order as controller-runtime's config loading:
//
//  1. KUBECONFIG, if set, must name exactly one valid kubeconfig file. It never falls back.
//  2. In a cluster (KUBERNETES_SERVICE_HOST set): the in-cluster ServiceAccount (returns "").
//  3. Otherwise $HOME/.kube/config, which must be valid.
//
// With none of these, it returns an error so the server does not start.
func resolveDelegationKubeconfig(getenv func(string) string, homeDir string) (string, error) {
	if getenv == nil {
		return "", fmt.Errorf("assertion failed: getenv must not be nil")
	}

	if raw := getenv(clientcmd.RecommendedConfigPathEnvVar); raw != "" {
		var paths []string
		for _, path := range filepath.SplitList(raw) {
			if strings.TrimSpace(path) != "" {
				paths = append(paths, path)
			}
		}
		if len(paths) != 1 {
			return "", fmt.Errorf("KUBECONFIG must name exactly one file for delegated authentication and authorization, got %d", len(paths))
		}
		if err := validateKubeconfigFile(paths[0]); err != nil {
			return "", err
		}
		return paths[0], nil
	}

	if getenv("KUBERNETES_SERVICE_HOST") != "" {
		return "", nil
	}

	if homeDir != "" {
		path := filepath.Join(homeDir, clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)
		if _, err := os.Stat(path); err == nil {
			if err := validateKubeconfigFile(path); err != nil {
				return "", err
			}
			return path, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("stat %s: %w", path, err)
		}
	}

	return "", fmt.Errorf("no Kubernetes configuration for delegated authentication and authorization: run in a cluster or set KUBECONFIG")
}

// validateKubeconfigFile loads the file with the non-deferred client config, which errors on
// empty or incomplete configs instead of silently falling back to in-cluster configuration the
// way the vendored options' deferred loader would.
func validateKubeconfigFile(path string) error {
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return fmt.Errorf("load kubeconfig %s for delegated authentication: %w", path, err)
	}
	// Resolve relative file references (tokenFile, certificate paths) against the kubeconfig's own
	// directory, as the vendored options' loader does, instead of the process working directory.
	if err := clientcmd.ResolveLocalPaths(cfg); err != nil {
		return fmt.Errorf("resolve paths in kubeconfig %s for delegated authentication: %w", path, err)
	}
	if _, err := clientcmd.NewDefaultClientConfig(*cfg, &clientcmd.ConfigOverrides{}).ClientConfig(); err != nil {
		return fmt.Errorf("invalid kubeconfig %s for delegated authentication: %w", path, err)
	}
	return nil
}
