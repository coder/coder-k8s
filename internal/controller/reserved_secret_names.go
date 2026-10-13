package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// reasonSecretNameReserved means another coder.com object in the namespace
// references or generates a Secret with the name a controller wants to
// claim for its output.
const reasonSecretNameReserved = "SecretNameReserved"

// reservedSecretName returns a SecretNameReserved failure when another
// coder.com object in namespace references or generates a Secret named name.
// Claiming such a name would hand that object a Secret it does not expect:
// another controller could read a gateway key as its operator token, or
// refuse its own Secret and stall. Kinds whose CRD is not installed are
// skipped. It lists each kind once, so it costs a few uncached reads.
func reservedSecretName(ctx context.Context, reader client.Reader, namespace, name string) error {
	if reader == nil || namespace == "" || name == "" {
		return errors.New("assertion failed: a reserved Secret name check needs a reader, a namespace and a name")
	}
	for _, source := range reservedSecretNameSources {
		users, err := source.users(ctx, reader, namespace, name)
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("list %s objects to check Secret name %q: %w", source.kind, name, err)
		}
		if len(users) > 0 {
			return &credentialsecret.Error{Reason: reasonSecretNameReserved, Message: fmt.Sprintf(
				"Secret name %q is used by %s %s. Choose another secretName.", name, source.kind, strings.Join(users, ", "))}
		}
	}
	return nil
}

type reservedSecretNameSource struct {
	kind  string
	users func(ctx context.Context, reader client.Reader, namespace, name string) ([]string, error)
}

var reservedSecretNameSources = []reservedSecretNameSource{
	{kind: "CoderControlPlane", users: secretUsers(
		func(l *coderv1alpha1.CoderControlPlaneList) []coderv1alpha1.CoderControlPlane { return l.Items }, controlPlaneSecretNames)},
	{kind: "CoderWorkspaceProxy", users: secretUsers(
		func(l *coderv1alpha1.CoderWorkspaceProxyList) []coderv1alpha1.CoderWorkspaceProxy { return l.Items },
		func(p *coderv1alpha1.CoderWorkspaceProxy) []string { return indexWorkspaceProxyBySecretNames(p) })},
	{kind: "CoderProvisioner", users: secretUsers(
		func(l *coderv1alpha1.CoderProvisionerList) []coderv1alpha1.CoderProvisioner { return l.Items },
		func(p *coderv1alpha1.CoderProvisioner) []string { return indexProvisionerBySecretNames(p) })},
	{kind: "CoderAIProvider", users: secretUsers(
		func(l *coderv1alpha1.CoderAIProviderList) []coderv1alpha1.CoderAIProvider { return l.Items },
		func(p *coderv1alpha1.CoderAIProvider) []string { return []string{p.Spec.APIKeySecretRef.Name} })},
	{kind: "CoderMCPServer", users: secretUsers(
		func(l *coderv1alpha1.CoderMCPServerList) []coderv1alpha1.CoderMCPServer { return l.Items }, mcpServerSecretNames)},
}

// secretUsers lists one kind in a namespace and returns the names of the
// objects whose Secret names include the given name. L is the list type and
// T the item type, so the compiler checks every conversion.
func secretUsers[L, T any, PL interface {
	*L
	client.ObjectList
}, PT interface {
	*T
	client.Object
}](items func(PL) []T, secretNames func(PT) []string) func(context.Context, client.Reader, string, string) ([]string, error) {
	return func(ctx context.Context, reader client.Reader, namespace, name string) ([]string, error) {
		list := PL(new(L))
		if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		var users []string
		all := items(list)
		for i := range all {
			obj := PT(&all[i])
			if slices.Contains(secretNames(obj), name) {
				users = append(users, fmt.Sprintf("%q", obj.GetName()))
			}
		}
		return users, nil
	}
}

// controlPlaneSecretNames returns every Secret name that a CoderControlPlane
// references or generates.
func controlPlaneSecretNames(cp *coderv1alpha1.CoderControlPlane) []string {
	names := []string{credentialsecret.OperatorTokenSecretName(cp)}
	if ref := cp.Status.OperatorTokenSecretRef; ref != nil {
		names = append(names, strings.TrimSpace(ref.Name))
	}
	names = append(names, indexByLicenseSecretName(cp)...)
	names = append(names, indexByDatabaseSecretName(cp)...)
	names = append(names, indexByEnvFromSecretName(cp)...)
	names = append(names, extraEnvSecretNames(cp.Spec.ExtraEnv)...)
	names = append(names, cp.Spec.TLS.SecretNames...)
	for _, ref := range cp.Spec.ImagePullSecrets {
		names = append(names, ref.Name)
	}
	for _, ref := range cp.Spec.Certs.Secrets {
		names = append(names, ref.Name)
	}
	if cp.Spec.Expose != nil && cp.Spec.Expose.Ingress != nil && cp.Spec.Expose.Ingress.TLS != nil {
		names = append(names, cp.Spec.Expose.Ingress.TLS.SecretName, cp.Spec.Expose.Ingress.TLS.WildcardSecretName)
	}
	for _, volume := range cp.Spec.Volumes {
		if volume.Secret != nil {
			names = append(names, volume.Secret.SecretName)
		}
	}
	return sortedUnique(names)
}

func mcpServerSecretNames(server *coderv1alpha1.CoderMCPServer) []string {
	auth := server.Spec.Auth
	var names []string
	if auth.APIKey != nil {
		names = append(names, auth.APIKey.SecretRef.Name)
	}
	for _, header := range auth.CustomHeaders {
		names = append(names, header.SecretRef.Name)
	}
	if auth.OAuth2 != nil {
		names = append(names, auth.OAuth2.ClientSecretRef.Name)
	}
	return sortedUnique(names)
}
