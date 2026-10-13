package controller

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

const (
	ownedSecretKind = "CoderAIGatewayKey"
	ownedSecretName = "gateway-key"
)

func ownedSecretScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, coderv1alpha1.AddToScheme(scheme))
	return scheme
}

func ownedSecretOwner(uid string) *coderv1alpha1.CoderAIGatewayKey {
	return &coderv1alpha1.CoderAIGatewayKey{
		TypeMeta:   metav1.TypeMeta{APIVersion: coderv1alpha1.GroupVersion.String(), Kind: ownedSecretKind},
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-" + uid, Namespace: "ns", UID: types.UID(uid)},
	}
}

func controllerRef(apiVersion, kind, uid string) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: "x", UID: types.UID(uid), Controller: ptrOf(true)}}
}

func ptrOf[T any](v T) *T { return &v }

func requireReason(t *testing.T, err error, reason string) {
	t.Helper()
	var credErr *credentialsecret.Error
	require.ErrorAs(t, err, &credErr)
	require.Equal(t, reason, credErr.Reason)
}

// Plan S1 at the helper: a Secret that this object does not control, or that
// is no longer a plain output Secret, is refused and left exactly as it was.
func TestOwnedSecretRefusesForeignSecrets(t *testing.T) {
	t.Parallel()
	owner := ownedSecretOwner("uid-1")
	gv := coderv1alpha1.GroupVersion.String()
	for name, secret := range map[string]*corev1.Secret{
		"no owner":                 {},
		"another object's UID":     {ObjectMeta: metav1.ObjectMeta{OwnerReferences: controllerRef(gv, ownedSecretKind, "uid-2")}},
		"another kind":             {ObjectMeta: metav1.ObjectMeta{OwnerReferences: controllerRef(gv, "CoderWorkspaceProxy", "uid-1")}},
		"another API group":        {ObjectMeta: metav1.ObjectMeta{OwnerReferences: controllerRef("example.com/v1", ownedSecretKind, "uid-1")}},
		"owner but not controller": {ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{APIVersion: gv, Kind: ownedSecretKind, Name: "x", UID: "uid-1"}}}},
		"not Opaque":               {ObjectMeta: metav1.ObjectMeta{OwnerReferences: controllerRef(gv, ownedSecretKind, "uid-1")}, Type: corev1.SecretTypeDockerConfigJson},
		"input credential label":   {ObjectMeta: metav1.ObjectMeta{OwnerReferences: controllerRef(gv, ownedSecretKind, "uid-1"), Labels: map[string]string{credentialsecret.LabelKey: "true"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			secret.Name, secret.Namespace = ownedSecretName, "ns"
			secret.Data = map[string][]byte{"token": []byte("someone-elses-value")}
			scheme := ownedSecretScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret.DeepCopy()).Build()
			before := &corev1.Secret{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), before))

			got, err := getOwnedSecret(context.Background(), c, owner, ownedSecretKind, ownedSecretName)
			require.Nil(t, got)
			requireReason(t, err, reasonSecretNotOwned)
			require.NotContains(t, err.Error(), "someone-elses-value")
			got, err = claimOwnedSecret(context.Background(), c, c, scheme, owner, ownedSecretKind, ownedSecretName, nil)
			require.Nil(t, got)
			requireReason(t, err, reasonSecretNotOwned)
			_, err = writeOwnedSecret(context.Background(), c, owner, ownedSecretKind, before.DeepCopy(), map[string][]byte{"key": []byte("new")}, nil, nil)
			require.ErrorContains(t, err, "assertion failed: refusing to write a Secret")

			after := &corev1.Secret{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), after))
			require.Equal(t, before, after, "a refused Secret must stay unchanged")
		})
	}
}

// An absent Secret is created empty and owned. A second claim returns the
// same Secret. A Secret that someone creates between the read and the create
// is refused, not adopted.
func TestClaimOwnedSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	owner := ownedSecretOwner("uid-1")
	scheme := ownedSecretScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	got, err := getOwnedSecret(ctx, c, owner, ownedSecretKind, ownedSecretName)
	require.NoError(t, err)
	require.Nil(t, got, "an absent Secret gives nil and no error")

	claimed, err := claimOwnedSecret(ctx, c, c, scheme, owner, ownedSecretKind, ownedSecretName, map[string]string{"app.kubernetes.io/instance": "gateway"})
	require.NoError(t, err)
	require.Equal(t, corev1.SecretTypeOpaque, claimed.Type)
	require.Empty(t, claimed.Data)
	require.Equal(t, map[string]string{"app.kubernetes.io/instance": "gateway", outputSecretManagedByKey: outputSecretManagedByValue}, claimed.Labels)
	require.True(t, isControlledBy(claimed, owner, ownedSecretKind))
	again, err := claimOwnedSecret(ctx, c, c, scheme, owner, ownedSecretKind, ownedSecretName, nil)
	require.NoError(t, err)
	require.Equal(t, claimed.UID, again.UID)

	// Someone else wins the race between the read and the create.
	race := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: obj.GetName(), Namespace: obj.GetNamespace()}}
			require.NoError(t, c.Create(ctx, foreign))
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	got, err = claimOwnedSecret(ctx, race, race, scheme, owner, ownedSecretKind, ownedSecretName, nil)
	require.Nil(t, got)
	requireReason(t, err, reasonSecretNotOwned)

	// Someone else creates the name and deletes it again before the re-read:
	// the claim must not report success without a Secret.
	vanish := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.GetName())
		},
	}).Build()
	got, err = claimOwnedSecret(ctx, vanish, vanish, scheme, owner, ownedSecretKind, ownedSecretName, nil)
	require.Nil(t, got)
	require.ErrorContains(t, err, "created and deleted by another writer")
	_, isCredErr := asCredentialError(err)
	require.False(t, isCredErr, "the race is retryable, not a stall")

	_, err = claimOwnedSecret(ctx, c, nil, scheme, owner, ownedSecretKind, ownedSecretName, nil)
	require.ErrorContains(t, err, "assertion failed: claiming an output Secret needs a client, a reader and a scheme")
	_, err = getOwnedSecret(ctx, nil, owner, ownedSecretKind, ownedSecretName)
	require.ErrorContains(t, err, "assertion failed: reading an output Secret needs a reader")
}

// A create that the API server or an admission check refuses gives
// SecretWriteRejected before any key exists. Other errors pass through.
func TestClaimOwnedSecretRejected(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Resource: "secrets"}
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"invalid name":       {err: apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, ownedSecretName, nil), reason: reasonSecretWriteRejected},
		"admission webhook":  {err: apierrors.NewForbidden(gr, ownedSecretName, errors.New("denied by policy")), reason: reasonSecretWriteRejected},
		"bad request":        {err: apierrors.NewBadRequest("bad"), reason: reasonSecretWriteRejected},
		"API server timeout": {err: apierrors.NewTimeoutError("slow", 1)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			scheme := ownedSecretScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return tc.err },
			}).Build()
			_, err := claimOwnedSecret(context.Background(), c, c, scheme, ownedSecretOwner("uid-1"), ownedSecretKind, ownedSecretName, nil)
			if tc.reason == "" {
				require.ErrorIs(t, err, tc.err)
				_, isCredErr := asCredentialError(err)
				require.False(t, isCredErr)
				return
			}
			requireReason(t, err, tc.reason)
		})
	}
}

// A write replaces the data, sets the controller's labels and annotations,
// keeps every other label and annotation, and fails on a stale read.
func TestWriteOwnedSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	owner := ownedSecretOwner("uid-1")
	scheme := ownedSecretScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	claimed, err := claimOwnedSecret(ctx, c, c, scheme, owner, ownedSecretKind, ownedSecretName, nil)
	require.NoError(t, err)

	// A reloader and Argo CD mark the Secret, and someone adds a data key.
	marked := claimed.DeepCopy()
	marked.Labels["argocd.argoproj.io/instance"] = "apps"
	marked.Annotations = map[string]string{"reloader.stakater.com/match": "true"}
	marked.Data = map[string][]byte{"extra": []byte("x")}
	require.NoError(t, c.Update(ctx, marked))
	current, err := getOwnedSecret(ctx, c, owner, ownedSecretKind, ownedSecretName)
	require.NoError(t, err)

	written, err := writeOwnedSecret(ctx, c, owner, ownedSecretKind, current,
		map[string][]byte{coderv1alpha1.CoderAIGatewayKeySecretDataKey: []byte("key-1")},
		map[string]string{"app.kubernetes.io/instance": "gateway"}, map[string]string{"coder.com/ai-gateway-key-name": "ck8s-x-1"})
	require.NoError(t, err)
	stored := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(written), stored))
	require.Equal(t, map[string][]byte{"key": []byte("key-1")}, stored.Data, "the write replaces all data")
	require.Equal(t, map[string]string{
		"app.kubernetes.io/instance": "gateway", "argocd.argoproj.io/instance": "apps", outputSecretManagedByKey: outputSecretManagedByValue,
	}, stored.Labels)
	require.Equal(t, map[string]string{"reloader.stakater.com/match": "true", "coder.com/ai-gateway-key-name": "ck8s-x-1"}, stored.Annotations)
	require.True(t, isControlledBy(stored, owner, ownedSecretKind))

	// current is now stale: the write must conflict, not overwrite.
	_, err = writeOwnedSecret(ctx, c, owner, ownedSecretKind, current, map[string][]byte{"key": []byte("key-2")}, nil, nil)
	require.True(t, apierrors.IsConflict(err), "want a conflict, got %v", err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(written), stored))
	require.Equal(t, []byte("key-1"), stored.Data["key"])

	// Options pass through: a dry run changes nothing.
	dryRun := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored.DeepCopy()).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(_ context.Context, _ client.WithWatch, _ client.Object, opts ...client.UpdateOption) error {
			update := &client.UpdateOptions{}
			update.ApplyOptions(opts)
			require.Equal(t, []string{metav1.DryRunAll}, update.DryRun)
			return nil
		},
	}).Build()
	_, err = writeOwnedSecret(ctx, dryRun, owner, ownedSecretKind, stored, map[string][]byte{"key": []byte("placeholder")}, nil, nil, client.DryRunAll)
	require.NoError(t, err)

	// A Secret from another namespace that carries a matching controller
	// reference is still not this owner's Secret.
	elsewhere := stored.DeepCopy()
	elsewhere.Namespace = "other"
	_, err = writeOwnedSecret(ctx, c, owner, ownedSecretKind, elsewhere, map[string][]byte{"key": []byte("key-3")}, nil, nil)
	require.ErrorContains(t, err, `assertion failed: output Secret "gateway-key" is in namespace "other", not in the owner's namespace "ns"`)

	_, err = writeOwnedSecret(ctx, c, owner, ownedSecretKind, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ownedSecretName, Namespace: "ns"}}, nil, nil, nil)
	require.ErrorContains(t, err, "assertion failed: output Secret \"gateway-key\" was not read before the write")
}

// The output Secret helper never adopts through CreateOrUpdate, which takes
// over an existing object (plan 5.3).
func TestOwnedSecretAvoidsCreateOrUpdate(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("owned_secret.go")
	require.NoError(t, err)
	require.False(t, strings.Contains(string(source), "CreateOrUpdate"), "owned_secret.go must not call CreateOrUpdate")
	require.False(t, strings.Contains(string(source), "CreateOrPatch"), "owned_secret.go must not call CreateOrPatch")
}
