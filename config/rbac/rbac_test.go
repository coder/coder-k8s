// Package rbac_test guards the operator's Secret access (plan #155 G2, S9):
// in config/rbac/*.yaml, the rules that can grant access to core Secrets and
// the bindings that can hand them out, and every file in
// config/apiserver-standalone/, must equal the golden copies in testdata/.
// The manifests are data files, so this package has no production code.
package rbac_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const (
	goldenSecretAccess = "testdata/secret-access.yaml" //nolint:gosec // A file path, not a credential.
	standaloneDir      = "../apiserver-standalone"
	goldenStandalone   = "testdata/apiserver-standalone"
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

// secretAccess is the part of config/rbac that decides who can reach core
// Secrets. Keys name the file, the kind, and the object.
type secretAccess struct {
	// Rules holds the Secret rules of each Role and ClusterRole that has any.
	Rules map[string][]rbacv1.PolicyRule `json:"rules"`
	// Bindings holds each binding of a role in Rules, and each binding of a
	// role that config/rbac does not define, whose rules this test cannot
	// see.
	Bindings map[string]binding `json:"bindings"`
}

type binding struct {
	RoleRef  rbacv1.RoleRef   `json:"roleRef"`
	Subjects []rbacv1.Subject `json:"subjects"`
}

// documents returns the YAML documents of path that hold an object.
func documents(t *testing.T, path string) [][]byte {
	t.Helper()
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(readFile(t, path))))
	var out [][]byte
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("split %s: %v", path, err)
		}
		var fields map[string]any
		if err := yaml.Unmarshal(doc, &fields); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if len(fields) > 0 {
			out = append(out, doc)
		}
	}
}

func decode(t *testing.T, path string, doc []byte, into any) {
	t.Helper()
	if err := yaml.UnmarshalStrict(doc, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// objectKey names a Role, a ClusterRole, or a binding: "ClusterRole name" or
// "Role namespace/name".
func objectKey(kind, namespace, name string) string {
	if namespace != "" {
		name = namespace + "/" + name
	}
	return kind + " " + name
}

// scanSecretAccess reads every object of config/rbac/*.yaml.
func scanSecretAccess(t *testing.T) secretAccess {
	t.Helper()
	paths, err := filepath.Glob("*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("assertion failed: config/rbac holds no manifest (%v)", err)
	}
	got := secretAccess{Rules: map[string][]rbacv1.PolicyRule{}, Bindings: map[string]binding{}}
	roles := map[string]string{} // objectKey -> key in got.Rules
	bindings := map[string]rbacv1.RoleBinding{}
	for _, path := range paths {
		for _, doc := range documents(t, path) {
			var object struct {
				metav1.TypeMeta   `json:",inline"`
				metav1.ObjectMeta `json:"metadata"`
			}
			if err := yaml.Unmarshal(doc, &object); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			key := objectKey(object.Kind, object.Namespace, object.Name)
			var rules []rbacv1.PolicyRule
			switch object.Kind {
			case "ClusterRole":
				var role rbacv1.ClusterRole
				decode(t, path, doc, &role)
				if role.AggregationRule != nil {
					t.Fatalf("assertion failed: %s holds aggregated %s, whose rules this test cannot see", path, key)
				}
				rules = role.Rules
			case "Role":
				var role rbacv1.Role
				decode(t, path, doc, &role)
				rules = role.Rules
			case "ClusterRoleBinding":
				var b rbacv1.ClusterRoleBinding
				decode(t, path, doc, &b)
				bindings[path+" "+key] = rbacv1.RoleBinding{RoleRef: b.RoleRef, Subjects: b.Subjects}
				continue
			case "RoleBinding":
				var b rbacv1.RoleBinding
				decode(t, path, doc, &b)
				bindings[path+" "+key] = b
				continue
			case "ServiceAccount":
				continue
			default:
				t.Fatalf("assertion failed: %s holds a %s, which this test does not check", path, object.Kind)
			}
			roles[key] = path + " " + key
			if rules := secretRules(rules); len(rules) > 0 {
				got.Rules[roles[key]] = rules
				if object.Kind == "ClusterRole" && len(object.Labels) > 0 {
					// The aggregationRule of any ClusterRole in the cluster can
					// select a label and copy these rules to its subjects.
					t.Errorf("%s grants Secret access and has labels %v, which can aggregate it into other ClusterRoles (plan #155 G2)", roles[key], object.Labels)
				}
			}
		}
	}
	for key, b := range bindings {
		namespace := ""
		if b.RoleRef.Kind == "Role" {
			namespace = b.Namespace
		}
		role, defined := roles[objectKey(b.RoleRef.Kind, namespace, b.RoleRef.Name)]
		if _, secrets := got.Rules[role]; !defined || secrets {
			got.Bindings[key] = binding{RoleRef: b.RoleRef, Subjects: b.Subjects}
		}
	}
	return got
}

func TestSecretAccessMatchesGolden(t *testing.T) {
	t.Parallel()
	var golden secretAccess
	if err := yaml.UnmarshalStrict(readFile(t, goldenSecretAccess), &golden); err != nil {
		t.Fatalf("parse %s: %v", goldenSecretAccess, err)
	}
	if len(golden.Rules) == 0 || len(golden.Bindings) == 0 {
		t.Fatalf("assertion failed: %s holds no rule or no binding", goldenSecretAccess)
	}
	got := scanSecretAccess(t)
	for key := range golden.Rules {
		if !reflect.DeepEqual(got.Rules[key], golden.Rules[key]) {
			t.Errorf("the Secret rules of %s changed (plan #155 G2).\ngot:  %+v\nwant: %+v", key, got.Rules[key], golden.Rules[key])
		}
	}
	for key := range got.Rules {
		if _, ok := golden.Rules[key]; !ok {
			t.Errorf("%s grants Secret access without a golden copy (plan #155 G2): %+v", key, got.Rules[key])
		}
	}
	for key := range golden.Bindings {
		if !reflect.DeepEqual(got.Bindings[key], golden.Bindings[key]) {
			t.Errorf("binding %s changed (plan #155 G2).\ngot:  %+v\nwant: %+v", key, got.Bindings[key], golden.Bindings[key])
		}
	}
	for key := range got.Bindings {
		if _, ok := golden.Bindings[key]; !ok {
			t.Errorf("binding %s can hand out Secret access without a golden copy (plan #155 G2): %+v", key, got.Bindings[key])
		}
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
