package apiservicetrust

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/coder/coder-k8s/internal/aggregated/servingcert"
)

const rbacManifest = "../../../config/rbac/apiservice-cabundle-role.yaml"

// shippedClusterRole returns the ClusterRole from the manifest users apply, so the test checks
// the real least-privilege rules rather than a copy.
func shippedClusterRole(t *testing.T) *rbacv1.ClusterRole {
	t.Helper()
	data, err := os.ReadFile(rbacManifest)
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewDocumentDecoder(io.NopCloser(bytes.NewReader(data)))
	defer func() { _ = decoder.Close() }()
	buf := make([]byte, len(data)+1)
	for {
		n, err := decoder.Read(buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal(buf[:n], &role); err != nil {
			t.Fatal(err)
		}
		if role.Kind == "ClusterRole" {
			return &role
		}
	}
	t.Fatalf("no ClusterRole in %s", rbacManifest)
	return nil
}

func TestEnvtestCABundleSyncWithShippedLeastPrivilegeRBAC(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set; run via `make test`")
	}
	env := &envtest.Environment{}
	adminCfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := t.Context()
	admin := kubernetes.NewForConfigOrDie(adminCfg)
	adminDyn := dynamic.NewForConfigOrDie(adminCfg)

	if _, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	manager, err := servingcert.NewManager(admin, testNS)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := manager.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString(bundle.CACertPEM)

	other := newAPIService("", true, nil)
	other.SetName("v1alpha1.other.example.com")
	if err := unstructured.SetNestedField(other.Object, "other.example.com", "spec", "group"); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []*unstructured.Unstructured{newAPIService("", true, nil), other} {
		if _, err := adminDyn.Resource(APIServiceGVR).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// The sync identity gets exactly the shipped ClusterRole, plus Secret read in its namespace
	// (which manager-role grants in production).
	user, err := env.AddUser(envtest.User{Name: "cabundle-sync"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	role := shippedClusterRole(t)
	role.ObjectMeta = metav1.ObjectMeta{Name: "test-apiservice-cabundle"}
	if _, err := admin.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	subject := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "cabundle-sync"}}
	if _, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-apiservice-cabundle"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   subject,
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.RbacV1().Roles(testNS).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "test-secret-read"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.RbacV1().RoleBindings(testNS).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-secret-read"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "test-secret-read"},
		Subjects:   subject,
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	userKube := kubernetes.NewForConfigOrDie(user.Config())
	userDyn := dynamic.NewForConfigOrDie(user.Config())

	// Least privilege: nothing beyond the one APIService.
	byName := fields.OneTermEqualSelector("metadata.name", APIServiceName).String()
	eventuallyErr(t, "RBAC to propagate", func() error {
		_, err := userDyn.Resource(APIServiceGVR).List(ctx, metav1.ListOptions{FieldSelector: byName})
		return err
	})
	if _, err := userDyn.Resource(APIServiceGVR).List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("list without the metadata.name selector must be forbidden, got %v", err)
	}
	if _, err := userDyn.Resource(APIServiceGVR).Get(ctx, other.GetName(), metav1.GetOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("get of another APIService must be forbidden, got %v", err)
	}
	if _, err := userDyn.Resource(APIServiceGVR).Patch(ctx, other.GetName(), types.MergePatchType, []byte(`{"spec":{"insecureSkipTLSVerify":false}}`), metav1.PatchOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("patch of another APIService must be forbidden, got %v", err)
	}

	controller, err := New(userDyn, userKube, testNS)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { controller.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	get := func() *unstructured.Unstructured {
		u, err := adminDyn.Resource(APIServiceGVR).Get(ctx, APIServiceName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	eventuallyTrue(t, 20*time.Second, "caBundle set and verification on", func() bool {
		u := get()
		_, hasInsecure, _ := unstructured.NestedBool(u.Object, "spec", "insecureSkipTLSVerify")
		return caBundleOf(u) == want && !hasInsecure
	})
	managed := false
	for _, mf := range get().GetManagedFields() {
		managed = managed || mf.Manager == FieldManager
	}
	if !managed {
		t.Fatalf("managedFields must name %q", FieldManager)
	}

	// Drift is repaired through the watch, without polling.
	if _, err := adminDyn.Resource(APIServiceGVR).Patch(ctx, APIServiceName, types.MergePatchType,
		[]byte(`{"spec":{"caBundle":"`+base64.StdEncoding.EncodeToString([]byte("wrong"))+`"}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, 10*time.Second, "drift repair", func() bool { return caBundleOf(get()) == want })

	// The other APIService was never touched.
	u, err := adminDyn.Resource(APIServiceGVR).Get(ctx, other.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !insecureOf(u) || caBundleOf(u) != "" {
		t.Fatal("another APIService must stay unchanged")
	}
}

func eventuallyTrue(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func eventuallyErr(t *testing.T, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := fn()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %v", what, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
