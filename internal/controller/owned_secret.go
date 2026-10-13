package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// Output Secret rules: a controller writes a Secret only when it is absent or
// when this object controls it. It never adopts or overwrites another Secret.
// Unlike the proxy's ensureTokenSecret, a write here sets only the
// controller's own labels and annotations and keeps every other one, because
// tools such as a reloader mark output Secrets on purpose.
const (
	// reasonSecretNotOwned means the output Secret exists and this object
	// does not control it, or it is no longer a plain output Secret.
	reasonSecretNotOwned = "SecretNotOwned"
	// reasonSecretWriteRejected means the API server or an admission check
	// refused to create the output Secret.
	reasonSecretWriteRejected = "SecretWriteRejected"

	outputSecretManagedByKey   = "app.kubernetes.io/managed-by" // #nosec G101 -- a label key, not a credential.
	outputSecretManagedByValue = "coder-k8s"
)

// checkOwnedSecret returns a SecretNotOwned failure unless owner (of kind)
// controls secret and secret is still a plain output Secret.
func checkOwnedSecret(secret *corev1.Secret, owner client.Object, kind string) *credentialsecret.Error {
	switch {
	case !isControlledBy(secret, owner, kind):
		return &credentialsecret.Error{Reason: reasonSecretNotOwned, Message: fmt.Sprintf(
			"Secret %q exists and this %s does not own it. The operator does not read or change it.", secret.Name, kind)}
	case secret.Type != "" && secret.Type != corev1.SecretTypeOpaque:
		return &credentialsecret.Error{Reason: reasonSecretNotOwned, Message: fmt.Sprintf(
			"Secret %q has type %q, not Opaque. The operator does not change it.", secret.Name, secret.Type)}
	case secret.Labels[credentialsecret.LabelKey] != "":
		return &credentialsecret.Error{Reason: reasonSecretNotOwned, Message: fmt.Sprintf(
			"Secret %q carries the label %s, so it is an input credential. The operator does not write it.", secret.Name, credentialsecret.LabelKey)}
	}
	return nil
}

// getOwnedSecret reads the Secret name in owner's namespace through reader,
// which callers set to a live (uncached) reader. It returns nil and no error
// when the Secret is absent, the Secret when owner controls it, and a
// SecretNotOwned failure otherwise.
func getOwnedSecret(ctx context.Context, reader client.Reader, owner client.Object, kind, name string) (*corev1.Secret, error) {
	if reader == nil {
		return nil, errors.New("assertion failed: reading an output Secret needs a reader")
	}
	if err := assertOutputSecretArgs(owner, kind, name); err != nil {
		return nil, err
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: owner.GetNamespace(), Name: name}, secret); apierrors.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("get output Secret %q: %w", name, err)
	}
	if failure := checkOwnedSecret(secret, owner, kind); failure != nil {
		return nil, failure
	}
	return secret, nil
}

// claimOwnedSecret makes sure the Secret name exists and that owner controls
// it. An absent Secret is created empty, with a controller ownerReference and
// labels, so ownership is settled before any key exists. A Secret that
// someone else created first gives SecretNotOwned. A create that the API
// server refuses gives SecretWriteRejected.
func claimOwnedSecret(ctx context.Context, c client.Client, reader client.Reader, scheme *runtime.Scheme, owner client.Object, kind, name string, labels map[string]string) (*corev1.Secret, error) {
	if c == nil || reader == nil || scheme == nil {
		return nil, errors.New("assertion failed: claiming an output Secret needs a client, a reader and a scheme")
	}
	secret, err := getOwnedSecret(ctx, reader, owner, kind, name)
	if err != nil || secret != nil {
		return secret, err
	}
	secret = &corev1.Secret{
		ObjectMeta: outputSecretMeta(owner.GetNamespace(), name, labels, nil),
		Type:       corev1.SecretTypeOpaque,
	}
	if err := controllerutil.SetControllerReference(owner, secret, scheme); err != nil {
		return nil, fmt.Errorf("set owner of output Secret %q: %w", name, err)
	}
	switch err := c.Create(ctx, secret); {
	case err == nil:
		return secret, nil
	case apierrors.IsAlreadyExists(err):
		// Created by someone else between the read and the create.
		existing, err := getOwnedSecret(ctx, reader, owner, kind, name)
		if err == nil && existing == nil {
			// ... and deleted again before this read. Nothing was claimed.
			return nil, fmt.Errorf("output Secret %q was created and deleted by another writer during the claim; retry", name)
		}
		return existing, err
	case apierrors.IsInvalid(err), apierrors.IsForbidden(err), apierrors.IsBadRequest(err):
		// The create held no data, so the API server's message holds none.
		return nil, &credentialsecret.Error{Reason: reasonSecretWriteRejected, Message: fmt.Sprintf(
			"The API server refused to create Secret %q: %v", name, err)}
	default:
		return nil, fmt.Errorf("create output Secret %q: %w", name, err)
	}
}

// writeOwnedSecret replaces the data of secret, a Secret that getOwnedSecret
// or claimOwnedSecret just returned, and sets labels and annotations next to
// the ones it already has. The update carries secret's resourceVersion, so a
// change since that read gives a conflict and the caller reads again. opts
// pass through, for example client.DryRunAll. Errors are returned as is and
// can hold the API server's message: callers must not put them into status
// or logs while data holds a credential.
func writeOwnedSecret(ctx context.Context, c client.Client, owner client.Object, kind string, secret *corev1.Secret, data map[string][]byte, labels, annotations map[string]string, opts ...client.UpdateOption) (*corev1.Secret, error) {
	if c == nil || secret == nil {
		return nil, errors.New("assertion failed: writing an output Secret needs a client and the Secret that was read")
	}
	if err := assertOutputSecretArgs(owner, kind, secret.Name); err != nil {
		return nil, err
	}
	if secret.Namespace != owner.GetNamespace() {
		return nil, fmt.Errorf("assertion failed: output Secret %q is in namespace %q, not in the owner's namespace %q", secret.Name, secret.Namespace, owner.GetNamespace())
	}
	if secret.ResourceVersion == "" {
		return nil, fmt.Errorf("assertion failed: output Secret %q was not read before the write", secret.Name)
	}
	if failure := checkOwnedSecret(secret, owner, kind); failure != nil {
		return nil, fmt.Errorf("assertion failed: refusing to write a Secret that this %s does not own: %s", kind, failure.Message)
	}
	updated := secret.DeepCopy()
	want := outputSecretMeta(secret.Namespace, secret.Name, labels, annotations)
	updated.Labels = mergeStringMaps(updated.Labels, want.Labels)
	updated.Annotations = mergeStringMaps(updated.Annotations, want.Annotations)
	updated.Data = maps.Clone(data)
	updated.StringData = nil
	if err := c.Update(ctx, updated, opts...); err != nil {
		return nil, err
	}
	return updated, nil
}

// outputSecretMeta returns the metadata the controller sets on an output
// Secret: labels, the managed-by label (so the credential Secret contract
// refuses the Secret as an input), and annotations.
func outputSecretMeta(namespace, name string, labels, annotations map[string]string) metav1.ObjectMeta {
	out := metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: maps.Clone(labels), Annotations: maps.Clone(annotations)}
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	out.Labels[outputSecretManagedByKey] = outputSecretManagedByValue
	return out
}

func mergeStringMaps(base, overlay map[string]string) map[string]string {
	if len(overlay) == 0 {
		return base
	}
	out := maps.Clone(base)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, overlay)
	return out
}

func assertOutputSecretArgs(owner client.Object, kind, name string) error {
	switch {
	case owner == nil || owner.GetUID() == "" || owner.GetNamespace() == "":
		return errors.New("assertion failed: an output Secret owner needs a UID and a namespace")
	case kind == "" || name == "":
		return errors.New("assertion failed: an output Secret needs an owner kind and a name")
	}
	return nil
}
