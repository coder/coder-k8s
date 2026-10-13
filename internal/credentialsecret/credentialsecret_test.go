package credentialsecret_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
	"github.com/coder/coder-k8s/internal/credentialsecret/credentialsecrettest"
)

const (
	testNamespace = "coder"
	testSecret    = "proxy-credentials" // #nosec G101 -- test fixture, not a credential.
	testKey       = "token"
	testValue     = "s3cr3t-Token-VALUE-4f9a"
	testOrigin    = "https://coder.example.com"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := coderv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add coder scheme: %v", err)
	}
	return scheme
}

func consentedSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            testSecret,
			Namespace:       testNamespace,
			UID:             "uid-1",
			ResourceVersion: "7",
			Labels:          map[string]string{credentialsecret.LabelKey: credentialsecret.LabelValue},
			Annotations:     map[string]string{credentialsecret.DestinationsAnnotation: testOrigin},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{testKey: []byte(testValue)},
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}

func TestRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mutate      func(*corev1.Secret)
		omitSecret  bool
		objects     []client.Object
		destination string
		key         string
		wantReason  string
	}{
		{name: "consented secret passes", destination: testOrigin},
		{name: "destination with path and default port passes", destination: "https://CODER.example.com:443/api/v2"},
		{name: "missing secret", omitSecret: true, destination: testOrigin, wantReason: credentialsecret.ReasonSecretNotFound},
		{
			name:        "no label",
			mutate:      func(s *corev1.Secret) { delete(s.Labels, credentialsecret.LabelKey) },
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonNotOptedIn,
		},
		{
			name:        "label not true",
			mutate:      func(s *corev1.Secret) { s.Labels[credentialsecret.LabelKey] = "yes" },
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonNotOptedIn,
		},
		{
			name:        "no destinations annotation",
			mutate:      func(s *corev1.Secret) { s.Annotations = nil },
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonNotOptedIn,
		},
		{
			name:        "destinations annotation with only separators",
			mutate:      func(s *corev1.Secret) { s.Annotations[credentialsecret.DestinationsAnnotation] = " , " },
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonMalformed,
		},
		{
			name:        "destination not listed",
			destination: "https://attacker.example.net",
			wantReason:  credentialsecret.ReasonDestinationNotAllowed,
		},
		{
			name:        "scheme differs",
			destination: "http://coder.example.com",
			wantReason:  credentialsecret.ReasonDestinationNotAllowed,
		},
		{
			name:        "port differs",
			destination: "https://coder.example.com:8443",
			wantReason:  credentialsecret.ReasonDestinationNotAllowed,
		},
		{
			name:        "subdomain is not the origin",
			destination: "https://evil.coder.example.com",
			wantReason:  credentialsecret.ReasonDestinationNotAllowed,
		},
		{
			name: "one of several destinations",
			mutate: func(s *corev1.Secret) {
				s.Annotations[credentialsecret.DestinationsAnnotation] = "https://a.example.com, https://coder.example.com\nhttp://b.example.com:8080"
			},
			destination: "http://b.example.com:8080",
		},
		{
			name: "destinations entry with a path is malformed",
			mutate: func(s *corev1.Secret) {
				s.Annotations[credentialsecret.DestinationsAnnotation] = "https://coder.example.com/api"
			},
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonMalformed,
		},
		{
			name:        "destinations entry without scheme is malformed",
			mutate:      func(s *corev1.Secret) { s.Annotations[credentialsecret.DestinationsAnnotation] = "coder.example.com" },
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonMalformed,
		},
		{
			name:        "key missing",
			key:         "other",
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonKeyMissing,
		},
		{
			name:        "empty value",
			mutate:      func(s *corev1.Secret) { s.Data[testKey] = nil },
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonMalformed,
		},
		{
			name:        "trailing newline",
			mutate:      func(s *corev1.Secret) { s.Data[testKey] = []byte(testValue + "\n") },
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonMalformed,
		},
		{
			name:        "leading space",
			mutate:      func(s *corev1.Secret) { s.Data[testKey] = []byte(" " + testValue) },
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonMalformed,
		},
		{
			name: "value larger than 8 KiB",
			mutate: func(s *corev1.Secret) {
				s.Data[testKey] = []byte(strings.Repeat("a", credentialsecret.MaxValueBytes+1))
			},
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonMalformed,
		},
		{
			name:        "value of exactly 8 KiB passes",
			mutate:      func(s *corev1.Secret) { s.Data[testKey] = []byte(strings.Repeat("a", credentialsecret.MaxValueBytes)) },
			destination: testOrigin,
		},
		{
			name:        "service account token type is refused",
			mutate:      func(s *corev1.Secret) { s.Type = corev1.SecretTypeServiceAccountToken },
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonRefused,
		},
		{
			name: "owned by a coder.com object is refused",
			mutate: func(s *corev1.Secret) {
				s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "coder.com/v1alpha1", Kind: "CoderWorkspaceProxy", Name: "other", UID: "uid-proxy"}}
			},
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonRefused,
		},
		{
			name: "owned by another group passes",
			mutate: func(s *corev1.Secret) {
				s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "example.io/v1", Kind: "Thing", Name: "thing", UID: "uid-thing"}}
			},
			destination: testOrigin,
		},
		{
			name:        "managed-by coder-k8s label is refused",
			mutate:      func(s *corev1.Secret) { s.Labels["app.kubernetes.io/managed-by"] = "coder-k8s" },
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonRefused,
		},
		{
			name:   "default operator token name of a control plane is refused",
			mutate: func(s *corev1.Secret) { s.Name = "coder-operator-token" },
			objects: []client.Object{&coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: testNamespace},
			}},
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonRefused,
		},
		{
			name:   "configured operator token name of a control plane is refused",
			mutate: func(s *corev1.Secret) { s.Name = "custom-token" },
			objects: []client.Object{&coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: testNamespace},
				Spec: coderv1alpha1.CoderControlPlaneSpec{
					OperatorAccess: coderv1alpha1.OperatorAccessSpec{GeneratedTokenSecretName: "custom-token"},
				},
			}},
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonRefused,
		},
		{
			name:   "operator token name in control plane status is refused",
			mutate: func(s *corev1.Secret) { s.Name = "status-token" },
			objects: []client.Object{&coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: testNamespace},
				Status: coderv1alpha1.CoderControlPlaneStatus{
					OperatorTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "status-token", Key: "token"},
				},
			}},
			destination: testOrigin,
			wantReason:  credentialsecret.ReasonRefused,
		},
		{
			name:   "control plane in another namespace does not refuse",
			mutate: func(s *corev1.Secret) { s.Name = "coder-operator-token" },
			objects: []client.Object{&coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "other"},
			}},
			destination: testOrigin,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			secret := consentedSecret()
			if tt.mutate != nil {
				tt.mutate(secret)
			}
			objects := append([]client.Object{}, tt.objects...)
			if !tt.omitSecret {
				objects = append(objects, secret)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()

			key := testKey
			if tt.key != "" {
				key = tt.key
			}
			ref := coderv1alpha1.SecretKeySelector{Name: secret.Name, Key: key}
			value, err := credentialsecret.Read(context.Background(), c, testNamespace, ref, mustURL(t, tt.destination))

			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				if value.Reveal() != string(secret.Data[key]) {
					t.Fatalf("expected the Secret value to be returned")
				}
				if value.Token.SecretName != secret.Name || value.Token.SecretKey != key {
					t.Fatalf("unexpected change token %+v", value.Token)
				}
				return
			}

			var credErr *credentialsecret.Error
			if !errors.As(err, &credErr) {
				t.Fatalf("expected a credential error with reason %s, got %v", tt.wantReason, err)
			}
			if credErr.Reason != tt.wantReason {
				t.Fatalf("expected reason %s, got %s (%s)", tt.wantReason, credErr.Reason, credErr.Message)
			}
			if !strings.Contains(credErr.Message, fmt.Sprintf("%q", secret.Name)) {
				t.Fatalf("expected message to name the Secret, got %q", credErr.Message)
			}
			credentialsecrettest.RequireNoLeak(t, []string{testValue}, map[string]any{"error": err})
		})
	}
}

func TestReadAssertions(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(consentedSecret()).Build()
	ref := coderv1alpha1.SecretKeySelector{Name: testSecret, Key: testKey}
	tests := []struct {
		name        string
		reader      client.Reader
		namespace   string
		ref         coderv1alpha1.SecretKeySelector
		destination *url.URL
	}{
		{name: "nil reader", namespace: testNamespace, ref: ref, destination: mustURL(t, testOrigin)},
		{name: "empty namespace", reader: c, ref: ref, destination: mustURL(t, testOrigin)},
		{name: "empty key", reader: c, namespace: testNamespace, ref: coderv1alpha1.SecretKeySelector{Name: testSecret}, destination: mustURL(t, testOrigin)},
		{name: "nil destination", reader: c, namespace: testNamespace, ref: ref},
		{name: "non-http destination", reader: c, namespace: testNamespace, ref: ref, destination: mustURL(t, "ftp://coder.example.com")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := credentialsecret.Read(context.Background(), tt.reader, tt.namespace, tt.ref, tt.destination)
			if err == nil || !strings.Contains(err.Error(), "assertion failed") {
				t.Fatalf("expected an assertion error, got %v", err)
			}
		})
	}
}

func TestOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{raw: "https://Coder.Example.com", want: "https://coder.example.com"},
		{raw: "https://coder.example.com:443/path?q=1", want: "https://coder.example.com"},
		{raw: "http://coder.example.com:80", want: "http://coder.example.com"},
		{raw: "http://coder.example.com:8080", want: "http://coder.example.com:8080"},
		{raw: "https://[::1]:8443", want: "https://[::1]:8443"},
		{raw: "ftp://coder.example.com", wantErr: true},
		{raw: "coder.example.com", wantErr: true},
		{raw: "https://", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			got, err := credentialsecret.Origin(mustURL(t, tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Origin(%q) = %q, %v; want %q", tt.raw, got, err, tt.want)
			}
		})
	}
}

func TestValueNeverPrintsSecret(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(consentedSecret()).Build()
	value, err := credentialsecret.Read(context.Background(), c, testNamespace,
		coderv1alpha1.SecretKeySelector{Name: testSecret, Key: testKey}, mustURL(t, testOrigin))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	encoded, err := json.Marshal(struct{ V credentialsecret.Value }{V: value})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text, err := value.MarshalText()
	if err != nil {
		t.Fatalf("marshal text: %v", err)
	}
	credentialsecrettest.RequireNoLeak(t, []string{testValue}, map[string]any{
		"%v":       fmt.Sprintf("%v", value),
		"%+v":      fmt.Sprintf("%+v", value),
		"%#v":      fmt.Sprintf("%#v", value),
		"%s":       fmt.Sprintf("%s", value),
		"%q":       fmt.Sprintf("%q", value),
		"%x":       fmt.Sprintf("%x", value),
		"pointer":  fmt.Sprintf("%v", &value),
		"json":     string(encoded),
		"text":     string(text),
		"marshal":  fmt.Sprint(value.MarshalLog()),
		"stringer": value.String(),
	})
}

func TestRedact(t *testing.T) {
	t.Parallel()

	secret := consentedSecret()
	secret.Data[testKey] = []byte("a+b/c=d?e&f")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	value, err := credentialsecret.Read(context.Background(), c, testNamespace,
		coderv1alpha1.SecretKeySelector{Name: testSecret, Key: testKey}, mustURL(t, testOrigin))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	raw := "a+b/c=d?e&f"
	var input strings.Builder
	for _, form := range credentialsecret.EncodedForms([]byte(raw)) {
		input.WriteString("before " + form + " after\n")
	}
	got := credentialsecret.Redact(input.String(), value)
	credentialsecrettest.RequireNoLeak(t, []string{raw}, map[string]any{"redacted": got})
	if !strings.Contains(got, "before [REDACTED] after") {
		t.Fatalf("expected redaction marker, got %q", got)
	}
	if credentialsecret.Redact("nothing here", value) != "nothing here" {
		t.Fatalf("expected text without values to stay unchanged")
	}
}

func TestWatchPredicate(t *testing.T) {
	t.Parallel()

	labeled := consentedSecret()
	unlabeled := consentedSecret()
	unlabeled.Labels = nil
	p := credentialsecret.WatchPredicate()

	if !p.Create(event.CreateEvent{Object: labeled}) {
		t.Fatalf("expected create of a labeled Secret to pass")
	}
	if p.Create(event.CreateEvent{Object: unlabeled}) {
		t.Fatalf("expected create of an unlabeled Secret to be dropped")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: unlabeled, ObjectNew: labeled}) {
		t.Fatalf("expected an update that adds the label to pass")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: labeled, ObjectNew: unlabeled}) {
		t.Fatalf("expected an update that removes the label to pass")
	}
	if p.Update(event.UpdateEvent{ObjectOld: unlabeled, ObjectNew: unlabeled}) {
		t.Fatalf("expected an update of an unlabeled Secret to be dropped")
	}
	if !p.Delete(event.DeleteEvent{Object: unlabeled}) {
		t.Fatalf("expected every delete to pass")
	}
	if p.Generic(event.GenericEvent{Object: labeled}) {
		t.Fatalf("expected generic events to be dropped")
	}
}
