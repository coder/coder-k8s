package coder

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestControlPlaneClientProviderClientForNamespaceHappyPath(t *testing.T) {
	t.Parallel()

	controlPlane := eligibleControlPlane("team-a", "coder")
	controlPlane.Status.OperatorTokenSecretRef.Key = "api-token"
	controlPlane.Status.URL = "https://coder.team-a.example.com"

	provider, secretReader := newControlPlaneProviderForTest(
		t,
		[]coderv1alpha1.CoderControlPlane{controlPlane},
		[]corev1.Secret{
			secretWithStringData("team-a", "operator-token", map[string]string{
				"api-token": "session-token",
			}),
		},
	)

	resolvedClient, err := provider.ClientForNamespace(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("resolve client: %v", err)
	}
	if resolvedClient == nil {
		t.Fatal("expected non-nil client")
	}
	if got, want := resolvedClient.SessionToken(), "session-token"; got != want {
		t.Fatalf("expected session token %q, got %q", want, got)
	}
	if got, want := resolvedClient.URL.String(), "https://coder.team-a.example.com"; got != want {
		t.Fatalf("expected URL %q, got %q", want, got)
	}
	if got, want := secretReader.getCalls, 1; got != want {
		t.Fatalf("expected %d secret read, got %d", want, got)
	}
}

func TestControlPlaneClientProviderClientForNamespaceSkipsDisabledControlPlaneWithoutSecretRead(t *testing.T) {
	t.Parallel()

	controlPlane := eligibleControlPlane("team-a", "coder")
	controlPlane.Spec.OperatorAccess.Disabled = true

	provider, secretReader := newControlPlaneProviderForTest(
		t,
		[]coderv1alpha1.CoderControlPlane{controlPlane},
		nil,
	)

	_, err := provider.ClientForNamespace(context.Background(), "team-a")
	if err == nil {
		t.Fatal("expected error")
	}
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected ServiceUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "no eligible CoderControlPlane") {
		t.Fatalf("expected no-eligible message, got %v", err)
	}
	if secretReader.getCalls != 0 {
		t.Fatalf("expected disabled control plane to skip secret reads, got %d", secretReader.getCalls)
	}
}

func TestControlPlaneClientProviderClientForNamespaceSkipsControlPlaneWithOperatorAccessNotReady(t *testing.T) {
	t.Parallel()

	controlPlane := eligibleControlPlane("team-a", "coder")
	controlPlane.Status.OperatorAccessReady = false

	provider, _ := newControlPlaneProviderForTest(
		t,
		[]coderv1alpha1.CoderControlPlane{controlPlane},
		nil,
	)

	_, err := provider.ClientForNamespace(context.Background(), "team-a")
	if err == nil {
		t.Fatal("expected error")
	}
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected ServiceUnavailable, got %v", err)
	}
}

func TestControlPlaneClientProviderClientForNamespaceSkipsControlPlaneWithNilSecretRef(t *testing.T) {
	t.Parallel()

	controlPlane := eligibleControlPlane("team-a", "coder")
	controlPlane.Status.OperatorTokenSecretRef = nil

	provider, _ := newControlPlaneProviderForTest(
		t,
		[]coderv1alpha1.CoderControlPlane{controlPlane},
		nil,
	)

	_, err := provider.ClientForNamespace(context.Background(), "team-a")
	if err == nil {
		t.Fatal("expected error")
	}
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected ServiceUnavailable, got %v", err)
	}
}

func TestControlPlaneClientProviderClientForNamespaceSkipsControlPlaneWithEmptyURL(t *testing.T) {
	t.Parallel()

	controlPlane := eligibleControlPlane("team-a", "coder")
	controlPlane.Status.URL = "   "

	provider, _ := newControlPlaneProviderForTest(
		t,
		[]coderv1alpha1.CoderControlPlane{controlPlane},
		nil,
	)

	_, err := provider.ClientForNamespace(context.Background(), "team-a")
	if err == nil {
		t.Fatal("expected error")
	}
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected ServiceUnavailable, got %v", err)
	}
}

func TestControlPlaneClientProviderClientForNamespaceSkipsControlPlaneWithDotInName(t *testing.T) {
	t.Parallel()

	controlPlane := eligibleControlPlane("team-a", "coder.control-plane")

	provider, _ := newControlPlaneProviderForTest(
		t,
		[]coderv1alpha1.CoderControlPlane{controlPlane},
		nil,
	)

	_, err := provider.ClientForNamespace(context.Background(), "team-a")
	if err == nil {
		t.Fatal("expected error")
	}
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected ServiceUnavailable, got %v", err)
	}
}

func TestControlPlaneClientProviderClientForNamespaceReturnsServiceUnavailableWhenNoEligibleControlPlane(t *testing.T) {
	t.Parallel()

	provider, _ := newControlPlaneProviderForTest(t, nil, nil)

	_, err := provider.ClientForNamespace(context.Background(), "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected ServiceUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "no eligible CoderControlPlane") {
		t.Fatalf("expected no-eligible message, got %v", err)
	}
}

func TestControlPlaneClientProviderClientForNamespaceReturnsBadRequestForMultipleEligibleControlPlanes(t *testing.T) {
	t.Parallel()

	provider, _ := newControlPlaneProviderForTest(
		t,
		[]coderv1alpha1.CoderControlPlane{
			eligibleControlPlane("team-a", "coder-a"),
			eligibleControlPlane("team-a", "coder-b"),
		},
		nil,
	)

	_, err := provider.ClientForNamespace(context.Background(), "team-a")
	if err == nil {
		t.Fatal("expected error")
	}
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest, got %v", err)
	}
	if !strings.Contains(err.Error(), "multi-instance support is planned") {
		t.Fatalf("expected multi-instance message, got %v", err)
	}
}

func TestControlPlaneClientProviderDefaultNamespaceHappyPath(t *testing.T) {
	t.Parallel()

	provider, secretReader := newControlPlaneProviderForTest(
		t,
		[]coderv1alpha1.CoderControlPlane{eligibleControlPlane("team-a", "coder")},
		nil,
	)

	resolvedNamespace, err := provider.DefaultNamespace(context.Background())
	if err != nil {
		t.Fatalf("resolve default namespace: %v", err)
	}
	if got, want := resolvedNamespace, "team-a"; got != want {
		t.Fatalf("expected default namespace %q, got %q", want, got)
	}
	if got, want := secretReader.getCalls, 0; got != want {
		t.Fatalf("expected %d secret reads, got %d", want, got)
	}
}

func TestControlPlaneClientProviderDefaultNamespaceReturnsServiceUnavailableWhenNoEligibleControlPlane(t *testing.T) {
	t.Parallel()

	provider, _ := newControlPlaneProviderForTest(t, nil, nil)

	_, err := provider.DefaultNamespace(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected ServiceUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "no eligible CoderControlPlane") {
		t.Fatalf("expected no-eligible message, got %v", err)
	}
}

func TestControlPlaneClientProviderDefaultNamespaceReturnsBadRequestForMultipleEligibleControlPlanes(t *testing.T) {
	t.Parallel()

	provider, _ := newControlPlaneProviderForTest(
		t,
		[]coderv1alpha1.CoderControlPlane{
			eligibleControlPlane("team-a", "coder-a"),
			eligibleControlPlane("team-b", "coder-b"),
		},
		nil,
	)

	_, err := provider.DefaultNamespace(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest, got %v", err)
	}
	if !strings.Contains(err.Error(), "multi-instance support is planned") {
		t.Fatalf("expected multi-instance message, got %v", err)
	}
}

func TestControlPlaneClientProviderClientForNamespaceDefaultsSecretKeyToToken(t *testing.T) {
	t.Parallel()

	controlPlane := eligibleControlPlane("team-a", "coder")
	controlPlane.Status.OperatorTokenSecretRef.Key = ""
	controlPlane.Status.URL = "https://coder.team-a.example.com"

	provider, _ := newControlPlaneProviderForTest(
		t,
		[]coderv1alpha1.CoderControlPlane{controlPlane},
		[]corev1.Secret{
			secretWithStringData("team-a", "operator-token", map[string]string{
				coderv1alpha1.DefaultTokenSecretKey: "default-token",
			}),
		},
	)

	resolvedClient, err := provider.ClientForNamespace(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("resolve client: %v", err)
	}
	if resolvedClient == nil {
		t.Fatal("expected non-nil client")
	}
	if got, want := resolvedClient.SessionToken(), "default-token"; got != want {
		t.Fatalf("expected session token %q, got %q", want, got)
	}
}

func newControlPlaneProviderForTest(
	t *testing.T,
	controlPlanes []coderv1alpha1.CoderControlPlane,
	secrets []corev1.Secret,
) (*ControlPlaneClientProvider, *countingReader) {
	t.Helper()

	scheme := newControlPlaneProviderTestScheme(t)

	cpReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithLists(&coderv1alpha1.CoderControlPlaneList{Items: controlPlanes}).
		Build()

	baseSecretReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithLists(&corev1.SecretList{Items: secrets}).
		Build()
	secretReader := &countingReader{Reader: baseSecretReader}

	provider, err := NewControlPlaneClientProvider(cpReader, secretReader, 10*time.Second)
	if err != nil {
		t.Fatalf("new control plane client provider: %v", err)
	}
	if provider == nil {
		t.Fatal("expected non-nil provider")
	}

	return provider, secretReader
}

func newControlPlaneProviderTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(coderv1alpha1.AddToScheme(scheme))

	return scheme
}

func eligibleControlPlane(namespace, name string) coderv1alpha1.CoderControlPlane {
	return coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Status: coderv1alpha1.CoderControlPlaneStatus{
			URL:                 "https://coder.example.com",
			OperatorAccessReady: true,
			OperatorTokenSecretRef: &coderv1alpha1.SecretKeySelector{
				Name: "operator-token",
				Key:  "token",
			},
		},
	}
}

func secretWithStringData(namespace, name string, data map[string]string) corev1.Secret {
	secretData := make(map[string][]byte, len(data))
	for key, value := range data {
		secretData[key] = []byte(value)
	}

	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Data: secretData,
	}
}

type countingReader struct {
	client.Reader
	getCalls int
}

func (r *countingReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	r.getCalls++
	return r.Reader.Get(ctx, key, obj, opts...)
}
