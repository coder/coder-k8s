// Package rbac_test guards the operator's Secret access (plan #155 G2, S9):
// the rules of config/rbac/role.yaml that can grant access to core Secrets,
// and every file in config/apiserver-standalone/, must equal the golden
// copies in testdata/. The manifests are data files, so this package has no
// production code.
package rbac_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const (
	rolePath          = "role.yaml"
	goldenSecretRules = "testdata/role-secrets-rules.yaml" //nolint:gosec // A file path, not a credential.
	standaloneDir     = "../apiserver-standalone"
	goldenStandalone  = "testdata/apiserver-standalone"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // Tests read fixed files of this repository.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// secretRules returns the rules that grant something on core Secrets: the
// core group or "*", and the resource secrets, a secrets subresource, or "*".
func secretRules(rules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
	var out []rbacv1.PolicyRule
	for _, rule := range rules {
		if !slices.Contains(rule.APIGroups, "") && !slices.Contains(rule.APIGroups, "*") {
			continue
		}
		if slices.ContainsFunc(rule.Resources, func(r string) bool {
			return r == "secrets" || r == "*" || strings.HasPrefix(r, "secrets/")
		}) {
			out = append(out, rule)
		}
	}
	return out
}

func TestRoleSecretRulesMatchGolden(t *testing.T) {
	t.Parallel()
	var role rbacv1.ClusterRole
	if err := yaml.UnmarshalStrict(readFile(t, rolePath), &role); err != nil {
		t.Fatalf("parse %s: %v", rolePath, err)
	}
	if role.Kind != "ClusterRole" || role.Name != "manager-role" {
		t.Fatalf("assertion failed: %s holds %s %q, want ClusterRole manager-role", rolePath, role.Kind, role.Name)
	}
	var golden []rbacv1.PolicyRule
	if err := yaml.UnmarshalStrict(readFile(t, goldenSecretRules), &golden); err != nil {
		t.Fatalf("parse %s: %v", goldenSecretRules, err)
	}
	if len(golden) == 0 {
		t.Fatalf("assertion failed: %s holds no rule", goldenSecretRules)
	}
	if got := secretRules(role.Rules); !reflect.DeepEqual(got, golden) {
		t.Fatalf("the Secret rules of %s changed (plan #155 G2).\ngot:  %+v\nwant: %+v", rolePath, got, golden)
	}
}

func TestAPIServerStandaloneMatchesGolden(t *testing.T) {
	t.Parallel()
	names := func(dir string) []string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}
	got, want := names(standaloneDir), names(goldenStandalone)
	if len(want) == 0 {
		t.Fatalf("assertion failed: %s holds no file", goldenStandalone)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the files of %s changed (plan #155 G2): got %v, want %v", standaloneDir, got, want)
	}
	for _, name := range want {
		if string(readFile(t, filepath.Join(standaloneDir, name))) != string(readFile(t, filepath.Join(goldenStandalone, name))) {
			t.Errorf("%s/%s differs from its golden copy (plan #155 G2)", standaloneDir, name)
		}
	}
}
