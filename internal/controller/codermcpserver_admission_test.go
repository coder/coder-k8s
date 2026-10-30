package controller_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	statusPolicyManifest = "../../config/admission/status-operator-only.yaml"
	operatorUser         = "system:serviceaccount:coder-system:coder-k8s"
	statusPolicyDenial   = "Only the coder-k8s operator ServiceAccount can write the status of CoderMCPServer objects."
)

// The opt-in /status policy in config/admission/ (plan S11, D11 A) denies a
// status write by a subject that RBAC allows, and allows the same write by
// the operator ServiceAccount. Both subjects are impersonated, so the
// envtest API server applies RBAC and the policy to them. The policy is
// cluster-wide and denies every CoderMCPServer status write that the
// operator ServiceAccount does not send, also those of other tests, so this
// test runs serially and waits until the policy is gone.
func TestCoderMCPServerStatusPolicy(t *testing.T) {
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "admission")
	server := &coderv1alpha1.CoderMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: namespace}, Spec: validMCPServerSpec()}
	require.NoError(t, k8sClient.Create(ctx, server))

	// RBAC lets both subjects write the status, so a denial comes from the
	// policy.
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "status-writer", Namespace: namespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"coder.com"}, Resources: []string{"codermcpservers"}, Verbs: []string{"get"}},
			{APIGroups: []string{"coder.com"}, Resources: []string{"codermcpservers/status"}, Verbs: []string{"update", "patch"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, role))
	require.NoError(t, k8sClient.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "status-writer", Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects: []rbacv1.Subject{
			{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "alice"},
			{Kind: rbacv1.ServiceAccountKind, Name: "coder-k8s", Namespace: "coder-system"},
		},
	}))
	alice := impersonatingClient(t, rest.ImpersonationConfig{UserName: "alice", Groups: []string{"system:authenticated"}})
	operator := impersonatingClient(t, rest.ImpersonationConfig{
		UserName: operatorUser,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:coder-system", "system:authenticated"},
	})
	writeStatus := func(c client.Client) error {
		got := &coderv1alpha1.CoderMCPServer{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(server), got); err != nil {
			return err
		}
		base := got.DeepCopy()
		got.Status.ObservedGeneration++
		return c.Status().Patch(ctx, got, client.MergeFrom(base))
	}
	require.NoError(t, writeStatus(alice), "RBAC allows alice before the policy exists")

	t.Cleanup(func() {
		// The API server drops the policy a moment after the delete. Cleanups run last
		// first, so this wait runs after the deletes below.
		require.Eventually(t, func() bool { return writeStatus(k8sClient) == nil }, 30*time.Second, 100*time.Millisecond, "the policy is gone")
	})
	for _, obj := range statusPolicyObjects(t) {
		require.NoError(t, k8sClient.Create(ctx, obj))
		t.Cleanup(func() {
			require.NoError(t, client.IgnoreNotFound(k8sClient.Delete(context.Background(), obj)))
		})
	}

	// The API server loads a new policy a moment after its create.
	var denial error
	require.Eventually(t, func() bool {
		denial = writeStatus(alice)
		return denial != nil && strings.Contains(denial.Error(), statusPolicyDenial)
	}, 30*time.Second, 100*time.Millisecond, "the policy denies alice")
	require.True(t, apierrors.IsForbidden(denial), "the denial is 403 Forbidden: %v", denial)
	require.NoError(t, writeStatus(operator), "the operator ServiceAccount writes the status")
	err := writeStatus(k8sClient)
	require.ErrorContains(t, err, statusPolicyDenial, "a cluster admin is denied too")
}

func impersonatingClient(t *testing.T, as rest.ImpersonationConfig) client.Client {
	t.Helper()
	impersonated := rest.CopyConfig(cfg)
	impersonated.Impersonate = as
	c, err := client.New(impersonated, client.Options{Scheme: scheme})
	require.NoError(t, err)
	return c
}

// statusPolicyObjects reads the shipped policy and binding unchanged.
func statusPolicyObjects(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(statusPolicyManifest)
	require.NoError(t, err)
	var objects []*unstructured.Unstructured
	for _, doc := range strings.Split(string(data), "\n---\n") {
		obj := map[string]any{}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &obj))
		objects = append(objects, &unstructured.Unstructured{Object: obj})
	}
	require.Len(t, objects, 2, "assertion failed: %s holds a policy and its binding", statusPolicyManifest)
	require.Equal(t, "ValidatingAdmissionPolicy", objects[0].GetKind())
	require.Equal(t, "ValidatingAdmissionPolicyBinding", objects[1].GetKind())
	return objects
}
