package apiservicetrust

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/coder/coder-k8s/internal/aggregated/servingcert"
)

const testNS = "coder-system"

type harness struct {
	dyn     *dynamicfake.FakeDynamicClient
	kube    *fake.Clientset
	ctrl    *Controller
	bundle  *servingcert.Bundle
	patches atomic.Int32
}

func newHarness(t *testing.T, apiService *unstructured.Unstructured, withSecret bool) *harness {
	t.Helper()
	h := &harness{kube: fake.NewClientset()}
	var objs []runtime.Object
	if apiService != nil {
		objs = append(objs, apiService)
	}
	h.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schemaGVR]string{APIServiceGVR: "APIServiceList"}, objs...)
	h.dyn.PrependReactor("patch", "apiservices", func(k8stesting.Action) (bool, runtime.Object, error) {
		h.patches.Add(1)
		return false, nil, nil
	})
	if withSecret {
		b, err := servingcert.Generate(testNS, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		h.bundle = b
		h.setSecret(t, b)
	}
	c, err := New(h.dyn, h.kube, testNS)
	if err != nil {
		t.Fatal(err)
	}
	h.ctrl = c
	return h
}

func (h *harness) setSecret(t *testing.T, b *servingcert.Bundle) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: servingcert.SecretName, Namespace: testNS},
		Type:       servingcert.SecretType,
		Data:       b.Data(),
	}
	if _, err := h.kube.CoreV1().Secrets(testNS).Get(context.Background(), servingcert.SecretName, metav1.GetOptions{}); err == nil {
		if _, err := h.kube.CoreV1().Secrets(testNS).Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := h.kube.CoreV1().Secrets(testNS).Create(context.Background(), secret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.ctrl.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("controller did not stop")
		}
	})
}

func (h *harness) get(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	u, err := h.dyn.Resource(APIServiceGVR).Get(context.Background(), APIServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func caBundleOf(u *unstructured.Unstructured) string {
	v, _, _ := unstructured.NestedString(u.Object, "spec", "caBundle")
	return v
}

func insecureOf(u *unstructured.Unstructured) bool {
	v, _, _ := unstructured.NestedBool(u.Object, "spec", "insecureSkipTLSVerify")
	return v
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// consistently fails if cond becomes false within d.
func consistently(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("%s changed", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newAPIService(caBundle string, insecure bool, annotations map[string]string) *unstructured.Unstructured {
	spec := map[string]any{
		"group": "aggregation.coder.com", "version": "v1alpha1",
		"service":              map[string]any{"name": servingcert.ServiceName, "namespace": testNS},
		"groupPriorityMinimum": int64(1000), "versionPriority": int64(100),
	}
	if insecure {
		spec["insecureSkipTLSVerify"] = true
	}
	if caBundle != "" {
		spec["caBundle"] = caBundle
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiregistration.k8s.io/v1", "kind": "APIService",
		"metadata": map[string]any{"name": APIServiceName},
		"spec":     spec,
	}}
	if annotations != nil {
		u.SetAnnotations(annotations)
	}
	return u
}

func patchActions(h *harness) []k8stesting.PatchActionImpl {
	var out []k8stesting.PatchActionImpl
	for _, a := range h.dyn.Actions() {
		if p, ok := a.(k8stesting.PatchActionImpl); ok {
			out = append(out, p)
		}
	}
	return out
}

func TestSyncPatchesInsecureAPIServiceOnceWithBothFields(t *testing.T) {
	h := newHarness(t, newAPIService("", true, nil), true)
	h.run(t)
	want := base64.StdEncoding.EncodeToString(h.bundle.CACertPEM)
	eventually(t, "caBundle", func() bool { return caBundleOf(h.get(t)) == want })
	if insecureOf(h.get(t)) {
		t.Fatal("insecureSkipTLSVerify must be off")
	}
	consistently(t, "patch count", 300*time.Millisecond, func() bool { return h.patches.Load() == 1 })
	p := patchActions(h)[0]
	if p.PatchType != types.MergePatchType || p.PatchOptions.FieldManager != FieldManager {
		t.Fatalf("patch type %s / field manager %q", p.PatchType, p.PatchOptions.FieldManager)
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(p.Patch, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || len(body["spec"]) != 2 || body["spec"]["caBundle"] != want || body["spec"]["insecureSkipTLSVerify"] != false {
		t.Fatalf("patch body = %s", p.Patch)
	}
}

func TestSyncLeavesMatchingAPIServiceAlone(t *testing.T) {
	b, err := servingcert.Generate(testNS, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, newAPIService(base64.StdEncoding.EncodeToString(b.CACertPEM), false, nil), false)
	h.setSecret(t, b)
	h.run(t)
	h.ctrl.Enqueue()
	consistently(t, "patch count", 500*time.Millisecond, func() bool { return h.patches.Load() == 0 })
}

func TestSyncPatchesWhenCABundleDiffersButInsecureOff(t *testing.T) {
	h := newHarness(t, newAPIService(base64.StdEncoding.EncodeToString([]byte("stale")), false, nil), true)
	h.run(t)
	want := base64.StdEncoding.EncodeToString(h.bundle.CACertPEM)
	eventually(t, "caBundle", func() bool { return caBundleOf(h.get(t)) == want })
}

func TestSyncRespectsOptOutAnnotation(t *testing.T) {
	h := newHarness(t, newAPIService("", true, map[string]string{OptOutAnnotation: "false"}), true)
	h.run(t)
	h.ctrl.Enqueue()
	consistently(t, "patch count", 500*time.Millisecond, func() bool { return h.patches.Load() == 0 })
	if !insecureOf(h.get(t)) || caBundleOf(h.get(t)) != "" {
		t.Fatal("an opted-out APIService must not change")
	}
}

func TestSyncManagesAgainWhenOptOutIsRemoved(t *testing.T) {
	h := newHarness(t, newAPIService("", true, map[string]string{OptOutAnnotation: "false"}), true)
	h.run(t)
	u := h.get(t)
	u.SetAnnotations(map[string]string{OptOutAnnotation: "true"})
	if _, err := h.dyn.Resource(APIServiceGVR).Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString(h.bundle.CACertPEM)
	eventually(t, "caBundle after removing the opt-out", func() bool { return caBundleOf(h.get(t)) == want })
}

func TestSyncWaitsForSecretThenPatchesOnEnqueue(t *testing.T) {
	h := newHarness(t, newAPIService("", true, nil), false)
	h.run(t)
	consistently(t, "patch count without Secret", 300*time.Millisecond, func() bool { return h.patches.Load() == 0 })
	b, err := servingcert.Generate(testNS, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.setSecret(t, b)
	h.ctrl.Enqueue() // what servingcert.Manager does after creating the Secret
	want := base64.StdEncoding.EncodeToString(b.CACertPEM)
	eventually(t, "caBundle after the Secret appears", func() bool { return caBundleOf(h.get(t)) == want })
}

func TestSyncNeverWritesAnInvalidSecret(t *testing.T) {
	h := newHarness(t, newAPIService("", true, nil), false)
	if _, err := h.kube.CoreV1().Secrets(testNS).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: servingcert.SecretName, Namespace: testNS},
		Type:       servingcert.SecretType,
		Data:       map[string][]byte{servingcert.CACertKey: []byte("-----BEGIN CERTIFICATE-----\nnot a cert\n-----END CERTIFICATE-----\n")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	h.ctrl.Enqueue()
	consistently(t, "patch count with a corrupt Secret", 500*time.Millisecond, func() bool { return h.patches.Load() == 0 })
}

func TestSyncWaitsForAPIServiceToBeCreated(t *testing.T) {
	h := newHarness(t, nil, true)
	h.run(t)
	consistently(t, "patch count without APIService", 300*time.Millisecond, func() bool { return h.patches.Load() == 0 })
	if _, err := h.dyn.Resource(APIServiceGVR).Create(context.Background(), newAPIService("", true, nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString(h.bundle.CACertPEM)
	eventually(t, "caBundle after creation", func() bool { return caBundleOf(h.get(t)) == want })
}

func TestSyncRepairsDrift(t *testing.T) {
	h := newHarness(t, newAPIService("", true, nil), true)
	h.run(t)
	want := base64.StdEncoding.EncodeToString(h.bundle.CACertPEM)
	eventually(t, "initial caBundle", func() bool { return caBundleOf(h.get(t)) == want })
	u := h.get(t)
	if err := unstructured.SetNestedField(u.Object, base64.StdEncoding.EncodeToString([]byte("wrong")), "spec", "caBundle"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.dyn.Resource(APIServiceGVR).Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "drift repair", func() bool { return caBundleOf(h.get(t)) == want })
}

func TestSyncFollowsCAChangesSignalledByTheManager(t *testing.T) {
	h := newHarness(t, newAPIService("", true, nil), true)
	h.run(t)
	eventually(t, "initial caBundle", func() bool { return h.patches.Load() == 1 })
	rotated, err := servingcert.Generate(testNS, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.setSecret(t, rotated)
	h.ctrl.Enqueue()
	want := base64.StdEncoding.EncodeToString(rotated.CACertPEM)
	eventually(t, "rotated caBundle", func() bool { return caBundleOf(h.get(t)) == want })
}

// TestSyncWritesTheSecretCAOnly models an old replica during a CA rotation: whatever a pod holds
// in memory, every controller writes the CA that is in the Secret now, so replicas cannot flap.
func TestSyncWritesTheSecretCAOnly(t *testing.T) {
	current, err := servingcert.Generate(testNS, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString(current.CACertPEM)
	h := newHarness(t, newAPIService(want, false, nil), false)
	h.setSecret(t, current)
	h.run(t)
	for i := 0; i < 5; i++ {
		h.ctrl.Enqueue()
	}
	consistently(t, "caBundle", 500*time.Millisecond, func() bool { return caBundleOf(h.get(t)) == want && h.patches.Load() == 0 })
}

func TestSyncRetriesAfterForbiddenWithoutGivingUp(t *testing.T) {
	h := newHarness(t, newAPIService("", true, nil), true)
	var forbidden atomic.Bool
	forbidden.Store(true)
	h.dyn.PrependReactor("patch", "apiservices", func(k8stesting.Action) (bool, runtime.Object, error) {
		if forbidden.Load() {
			h.patches.Add(1) // this reactor runs before the counting one
			return true, nil, apierrors.NewForbidden(APIServiceGVR.GroupResource(), APIServiceName, errFake("rbac"))
		}
		return false, nil, nil
	})
	h.run(t)
	eventually(t, "retries", func() bool { return h.patches.Load() >= 3 })
	forbidden.Store(false)
	want := base64.StdEncoding.EncodeToString(h.bundle.CACertPEM)
	eventually(t, "patch after RBAC is fixed", func() bool { return caBundleOf(h.get(t)) == want })
}

func TestWarnIsRateLimitedPerKind(t *testing.T) {
	c := &Controller{lastWarn: map[string]time.Time{}}
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		c.warn("forbidden", nil, "x")
	}
	if got := c.lastWarn["forbidden"]; !got.Equal(now) {
		t.Fatalf("first warning must be recorded at %v, got %v", now, got)
	}
	now = now.Add(warnInterval - time.Second)
	c.warn("forbidden", nil, "x")
	if !c.lastWarn["forbidden"].Equal(time.Unix(1000, 0)) {
		t.Fatal("a repeated warning inside the interval must be suppressed")
	}
	now = now.Add(2 * time.Second)
	c.warn("forbidden", nil, "x")
	if !c.lastWarn["forbidden"].Equal(now) {
		t.Fatal("a warning after the interval must be logged again")
	}
	c.warn("absent", nil, "y")
	if !c.lastWarn["absent"].Equal(now) {
		t.Fatal("each kind of warning is limited separately")
	}
}

func TestNewAssertions(t *testing.T) {
	if _, err := New(nil, fake.NewClientset(), testNS); err == nil {
		t.Fatal("expected assertion for nil dynamic client")
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schemaGVR]string{APIServiceGVR: "APIServiceList"})
	if _, err := New(dyn, fake.NewClientset(), ""); err == nil {
		t.Fatal("expected assertion for empty namespace")
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

type schemaGVR = schema.GroupVersionResource
