package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/servingcert"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// The reserved-name check lists these kinds with a live reader, so it needs
// list but no watch.
// +kubebuilder:rbac:groups=coder.com,resources=coderaiproviders;codermcpservers;coderaigatewaykeys,verbs=list

// reasonSecretNameReserved means another coder.com object in the namespace
// references or generates a Secret with the name a controller wants to
// claim for its output.
const reasonSecretNameReserved = "SecretNameReserved"

// reservedSecretName returns a SecretNameReserved failure when another
// coder.com object in owner's namespace references or generates a Secret
// named name. Claiming such a name would hand that object a Secret it does
// not expect: another controller could read a gateway key as its operator
// token, or refuse its own Secret and stall. Among CoderAIGatewayKey objects
// with the same secretName, the oldest one keeps the name. Kinds whose CRD is
// not installed are skipped. It lists each kind once, so it costs a few
// uncached reads.
func reservedSecretName(ctx context.Context, reader client.Reader, owner client.Object, name string) error {
	if reader == nil || owner == nil || owner.GetNamespace() == "" || owner.GetName() == "" || name == "" {
		return errors.New("assertion failed: a reserved Secret name check needs a reader, an owner with a name and namespace, and a Secret name")
	}
	if name == servingcert.SecretName {
		// The aggregated API server can run in any namespace, so its fixed
		// serving Secret name is reserved in every namespace (plan D13).
		return &credentialsecret.Error{Reason: reasonSecretNameReserved, Message: fmt.Sprintf(
			"Secret name %q is the serving Secret of the coder-k8s aggregated API server. Choose another secretName.", name)}
	}
	for _, source := range reservedSecretNameSources {
		users, err := source.users(ctx, reader, owner, name)
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
	users func(ctx context.Context, reader client.Reader, owner client.Object, name string) ([]string, error)
}

var reservedSecretNameSources = []reservedSecretNameSource{
	{kind: "CoderControlPlane", users: secretUsers(
		func(l *coderv1alpha1.CoderControlPlaneList) []coderv1alpha1.CoderControlPlane { return l.Items }, controlPlaneSecretNames)},
	{kind: "CoderWorkspaceProxy", users: secretUsers(
		func(l *coderv1alpha1.CoderWorkspaceProxyList) []coderv1alpha1.CoderWorkspaceProxy { return l.Items },
		workspaceProxySecretNames)},
	{kind: "CoderProvisioner", users: secretUsers(
		func(l *coderv1alpha1.CoderProvisionerList) []coderv1alpha1.CoderProvisioner { return l.Items },
		provisionerSecretNames)},
	{kind: "CoderAIProvider", users: secretUsers(
		func(l *coderv1alpha1.CoderAIProviderList) []coderv1alpha1.CoderAIProvider { return l.Items },
		func(p *coderv1alpha1.CoderAIProvider) []string { return []string{p.Spec.APIKeySecretRef.Name} })},
	{kind: "CoderMCPServer", users: secretUsers(
		func(l *coderv1alpha1.CoderMCPServerList) []coderv1alpha1.CoderMCPServer { return l.Items }, mcpServerSecretNames)},
	{kind: "CoderAIGatewayKey", users: secretUsersFor(
		func(l *coderv1alpha1.CoderAIGatewayKeyList) []coderv1alpha1.CoderAIGatewayKey { return l.Items }, olderGatewayKeySecretName)},
}

// olderGatewayKeySecretName returns key's output Secret name when key is a
// CoderAIGatewayKey that is older than owner (by creation time, then name).
// The oldest object keeps a shared name, so two objects never reserve it
// from each other. No object is older than itself, so owner never reserves
// its own name.
func olderGatewayKeySecretName(owner client.Object, key *coderv1alpha1.CoderAIGatewayKey) []string {
	ownerCreated, keyCreated := owner.GetCreationTimestamp(), key.CreationTimestamp
	older := keyCreated.Before(&ownerCreated) || (keyCreated.Equal(&ownerCreated) && key.Name < owner.GetName())
	if !older {
		return nil
	}
	return []string{key.Spec.SecretName}
}

// secretUsers is secretUsersFor for kinds whose Secret names do not depend
// on the owner.
func secretUsers[L, T any, PL interface {
	*L
	client.ObjectList
}, PT interface {
	*T
	client.Object
}](items func(PL) []T, secretNames func(PT) []string) func(context.Context, client.Reader, client.Object, string) ([]string, error) {
	return secretUsersFor[L, T, PL, PT](items, func(_ client.Object, obj PT) []string { return secretNames(obj) })
}

// secretUsersFor lists one kind in owner's namespace and returns the names of
// the objects whose Secret names include the given name. Names are compared
// after trimming spaces, as the controllers that use them trim. L is the list
// type and T the item type, so the compiler checks every conversion.
func secretUsersFor[L, T any, PL interface {
	*L
	client.ObjectList
}, PT interface {
	*T
	client.Object
}](items func(PL) []T, secretNames func(client.Object, PT) []string) func(context.Context, client.Reader, client.Object, string) ([]string, error) {
	return func(ctx context.Context, reader client.Reader, owner client.Object, name string) ([]string, error) {
		list := PL(new(L))
		if err := reader.List(ctx, list, client.InNamespace(owner.GetNamespace())); err != nil {
			return nil, err
		}
		var users []string
		all := items(list)
		for i := range all {
			obj := PT(&all[i])
			if slices.ContainsFunc(secretNames(owner, obj), func(n string) bool { return strings.TrimSpace(n) == name }) {
				users = append(users, fmt.Sprintf("%q", obj.GetName()))
			}
		}
		return users, nil
	}
}

// controlPlaneSecretNames returns every Secret name that a CoderControlPlane
// references or generates: the names that credentialsecret.Read refuses
// because of cp.
//
// The operator token name stays reserved while operator access is disabled:
// turning it back on makes the control plane create that Secret again.
func controlPlaneSecretNames(cp *coderv1alpha1.CoderControlPlane) []string {
	return credentialsecret.ControlPlaneSecretNames(cp)
}

// workspaceProxySecretNames adds the image pull Secrets and the recorded
// token Secret to the names that the proxy's credential index returns.
func workspaceProxySecretNames(proxy *coderv1alpha1.CoderWorkspaceProxy) []string {
	names := indexWorkspaceProxyBySecretNames(proxy)
	names = append(names, localRefNames(proxy.Spec.ImagePullSecrets)...)
	if ref := proxy.Status.ProxyTokenSecretRef; ref != nil {
		names = append(names, ref.Name)
	}
	return sortedUnique(names)
}

// provisionerSecretNames adds the image pull Secrets and the recorded key
// Secret to the names that the provisioner's credential index returns.
func provisionerSecretNames(provisioner *coderv1alpha1.CoderProvisioner) []string {
	names := indexProvisionerBySecretNames(provisioner)
	names = append(names, localRefNames(provisioner.Spec.ImagePullSecrets)...)
	if ref := provisioner.Status.SecretRef; ref != nil {
		names = append(names, ref.Name)
	}
	return sortedUnique(names)
}

func localRefNames(refs []corev1.LocalObjectReference) []string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	return names
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
