// Package promotion_test checks the GitOps promotion example: it renders the
// directory with kustomize, as Argo CD and Flux do, and runs promote.sh
// against a fake kubectl. The example has no Go code.
package promotion_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

const (
	versionID = "9b2f6c1e-4d3a-4f7e-8a51-0c6d2e9f1b73"
	// maxTestTimeoutSeconds is the API maximum of spec.timeoutSeconds.
	maxTestTimeoutSeconds = 7200
	startGraceSeconds     = 900
	pollSeconds           = 30
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// rendered holds the objects of `kustomize build` of the example.
type rendered struct {
	test        map[string]any
	job         batchv1.Job
	role        rbacv1.Role
	roleBinding rbacv1.RoleBinding
	configMap   corev1.ConfigMap
	// namespaces maps each object ID to its namespace.
	namespaces map[string]string
}

func render(t *testing.T, fs filesys.FileSystem) rendered {
	t.Helper()
	resMap, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fs, ".")
	if err != nil {
		t.Fatalf("kustomize build: %v", err)
	}
	out := rendered{namespaces: map[string]string{}}
	for _, res := range resMap.Resources() {
		out.namespaces[res.CurId().String()] = res.GetNamespace()
		data, err := res.AsYAML()
		if err != nil {
			t.Fatalf("render %s: %v", res.CurId(), err)
		}
		var target any
		switch res.GetKind() {
		case "CoderTemplateTest":
			target = &out.test
		case "Job":
			target = &out.job
		case "Role":
			target = &out.role
		case "RoleBinding":
			target = &out.roleBinding
		case "ConfigMap":
			target = &out.configMap
		default:
			continue
		}
		if err := yaml.Unmarshal(data, target); err != nil {
			t.Fatalf("decode %s: %v", res.CurId(), err)
		}
	}
	if out.test == nil || out.job.Name == "" || out.role.Name == "" || out.roleBinding.Name == "" || out.configMap.Name == "" {
		t.Fatal("assertion failed: the example must render a test, a Job, a Role, a RoleBinding, and a ConfigMap")
	}
	return out
}

func runScript(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("promote.sh needs jq on PATH")
	}
	cmd := exec.Command("bash", append([]string{"promote.sh"}, args...)...) //nolint:gosec // Tests run the example script with fixed arguments.
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func objectName(t *testing.T, prefix, version string) string {
	t.Helper()
	out, err := runScript(t, nil, "name", prefix, version)
	if err != nil {
		t.Fatalf("promote.sh name %s %s: %v: %s", prefix, version, err, out)
	}
	return strings.TrimSpace(out)
}

func TestObjectNames(t *testing.T) {
	long := strings.Repeat("Ab_", 21) + "c"
	if len(long) != 64 {
		t.Fatalf("assertion failed: want a 64-character version, got %d", len(long))
	}
	seen := map[string]string{}
	for _, version := range []string{"v2", "V2", "v2.0", "v2_0", "v2-0", "V2.0_rc1", long} {
		name := objectName(t, "docker", version)
		if len(name) > 63 || !dnsLabel.MatchString(name) || !strings.HasPrefix(name, "docker-") {
			t.Errorf("version %q: name %q is not a DNS label with prefix docker-", version, name)
		}
		if other, ok := seen[name]; ok {
			t.Errorf("versions %q and %q both map to %q", other, version, name)
		}
		seen[name] = version
	}
	if got := objectName(t, "docker", "V2.0_rc1"); !strings.HasPrefix(got, "docker-v2-0-rc1-") {
		t.Errorf("V2.0_rc1 maps to %q, want the readable prefix docker-v2-0-rc1-", got)
	}
	for _, version := range []string{"", "v2..0", "-v2", "v2/0", strings.Repeat("a", 65)} {
		if out, err := runScript(t, nil, "name", "docker", version); err == nil {
			t.Errorf("version %q: want an error, got %q", version, out)
		}
	}
}

func TestExampleWiring(t *testing.T) {
	r := render(t, filesys.MakeFsOnDisk())
	spec := r.test["spec"].(map[string]any)
	name := r.test["metadata"].(map[string]any)["name"].(string)
	version := spec["version"].(map[string]any)["name"].(string)
	if want := objectName(t, "docker", version); name != want {
		t.Errorf("test name %q, want %q from promote.sh name docker %s", name, want, version)
	}
	if r.job.Name != name {
		t.Errorf("Job name %q, want the test name %q", r.job.Name, name)
	}
	if got := r.role.Rules[0].ResourceNames; len(got) != 1 || got[0] != name {
		t.Errorf("Role may get tests %v, want only %q", got, name)
	}
	if got := r.role.Rules[1].ResourceNames; len(got) != 1 || got[0] != spec["template"] {
		t.Errorf("Role may promote %v, want only %v", got, spec["template"])
	}

	for _, file := range []string{"promote.sh", "codertemplatetest.json"} {
		disk, err := os.ReadFile(file) //nolint:gosec // Tests read fixed files of this example.
		if err != nil {
			t.Fatal(err)
		}
		if r.configMap.Data[file] != string(disk) {
			t.Errorf("ConfigMap %s holds another %s than the file", r.configMap.Name, file)
		}
	}
	volumes := r.job.Spec.Template.Spec.Volumes
	if volumes[0].ConfigMap == nil || volumes[0].ConfigMap.Name != r.configMap.Name {
		t.Errorf("Job mounts %+v, want ConfigMap %s", volumes[0], r.configMap.Name)
	}

	if b := r.job.Spec.BackoffLimit; b == nil || *b != 0 {
		t.Errorf("Job backoffLimit %v, want 0: a pod retry would promote again without reading the active version", b)
	}
	deadline := r.job.Spec.ActiveDeadlineSeconds
	if deadline == nil || *deadline <= maxTestTimeoutSeconds+startGraceSeconds {
		t.Errorf("Job activeDeadlineSeconds %v, want above %d", deadline, maxTestTimeoutSeconds+startGraceSeconds)
	}
	testWave := r.test["metadata"].(map[string]any)["annotations"].(map[string]any)["argocd.argoproj.io/sync-wave"].(string)
	if atoi(t, testWave) >= atoi(t, r.job.Annotations["argocd.argoproj.io/sync-wave"]) || r.job.Annotations["argocd.argoproj.io/hook"] != "Sync" {
		t.Errorf("Job annotations %v: want an Argo CD Sync hook in a later wave than the test (%s)", r.job.Annotations, testWave)
	}

	var flux struct {
		Spec struct {
			Wait    bool   `json:"wait"`
			Force   bool   `json:"force"`
			Timeout string `json:"timeout"`
		} `json:"spec"`
	}
	data, err := os.ReadFile("flux-kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &flux); err != nil {
		t.Fatal(err)
	}
	timeout, err := time.ParseDuration(flux.Spec.Timeout)
	if err != nil {
		t.Fatal(err)
	}
	if !flux.Spec.Wait || deadline == nil || timeout <= time.Duration(*deadline)*time.Second {
		t.Errorf("Flux wait %v and timeout %s: want wait and a timeout above the Job deadline", flux.Spec.Wait, timeout)
	}
	if !flux.Spec.Force {
		t.Error("Flux force is off: a change of promote.sh would leave the Job unchangeable")
	}
}

// TestCopyForAnotherTemplate renders a copy whose test names another template
// in another namespace.
func TestCopyForAnotherTemplate(t *testing.T) {
	fs := filesys.MakeFsInMemory()
	for _, file := range []string{"kustomization.yaml", "rbac.yaml", "promote-job.yaml", "promote.sh", "codertemplatetest.json"} {
		data, err := os.ReadFile(file) //nolint:gosec // Tests read fixed files of this example.
		if err != nil {
			t.Fatal(err)
		}
		if file == "codertemplatetest.json" {
			data = []byte(strings.NewReplacer(`"namespace": "coder"`, `"namespace": "ci"`, `"default.docker"`, `"acme.web"`).Replace(string(data)))
		}
		if err := fs.WriteFile(file, data); err != nil {
			t.Fatal(err)
		}
	}
	r := render(t, fs)
	if r.test["metadata"].(map[string]any)["namespace"] != "ci" {
		t.Fatal("assertion failed: the copy must move the test to namespace ci")
	}
	for id, ns := range r.namespaces {
		if ns != "ci" {
			t.Errorf("%s is in namespace %q, want the test namespace ci", id, ns)
		}
	}
	if got := r.roleBinding.Subjects[0].Namespace; got != "ci" {
		t.Errorf("RoleBinding subject namespace %q, want ci", got)
	}
	if got := r.role.Rules[1].ResourceNames; len(got) != 1 || got[0] != "acme.web" {
		t.Errorf("Role may promote %v, want only the test template acme.web", got)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// fakeKubectl answers the Nth `get` with the reply whose key is the largest
// one at most N, and records each promotion body. Fake date and sleep share
// one clock, so a two-hour wait takes no real time.
const fakeKubectl = `#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"$FAKE/calls"
case "$*" in
*" get codertemplatetest "*)
  n=0
  [[ -f $FAKE/gets ]] && read -r n <"$FAKE/gets"
  n=$((n + 1))
  echo "$n" >"$FAKE/gets"
  best=0
  for f in "$FAKE"/get-*; do
    k=${f##*/get-}
    if ((k <= n && k > best)); then best=$k; fi
  done
  reply=$(cat "$FAKE/get-$best")
  if [[ $reply == NotFound ]]; then
    echo 'Error from server (NotFound): codertemplatetests.coder.com not found' >&2
    exit 1
  fi
  echo "$reply"
  ;;
"create --raw "*)
  cat >>"$FAKE/promotions"
  echo >>"$FAKE/promotions"
  cat "$FAKE/promote-answer"
  exit "$(cat "$FAKE/promote-exit")"
  ;;
*) echo "fake kubectl: unexpected call: $*" >&2; exit 2 ;;
esac
`

const fakeDate = `#!/usr/bin/env bash
[[ $1 == +%s ]] || { echo "fake date: unexpected $*" >&2; exit 2; }
read -r now <"$FAKE/now"
echo "$now"
`

const fakeSleep = `#!/usr/bin/env bash
read -r now <"$FAKE/now"
echo $((now + $1)) >"$FAKE/now"
`

type promoteCase struct {
	name string
	// want is the test manifest in Git. Nil means the example's file.
	want map[string]any
	// gets maps the number of a get, starting at 1, to the object in the
	// cluster from then on. Nil means NotFound.
	gets          map[int]map[string]any
	promoteAnswer string
	promoteFails  bool

	wantOK         bool
	wantPromotions int
	wantOutput     string
	// wantMinGets checks that the script waited long enough.
	wantMinGets int
}

func runPromote(t *testing.T, c promoteCase, example map[string]any) (output string, ok bool, promotions []string, gets int) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake")
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{fake, bin} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(bin, "kubectl"):         fakeKubectl,
		filepath.Join(bin, "date"):            fakeDate,
		filepath.Join(bin, "sleep"):           fakeSleep,
		filepath.Join(fake, "now"):            "1000000\n",
		filepath.Join(fake, "promote-exit"):   "0\n",
		filepath.Join(fake, "promote-answer"): c.promoteAnswer,
	}
	if c.promoteFails {
		files[filepath.Join(fake, "promote-exit")] = "1\n"
	}
	want := c.want
	if want == nil {
		want = example
	}
	files[filepath.Join(dir, "want.json")] = mustJSON(t, want)
	for n, obj := range c.gets {
		reply := "NotFound"
		if obj != nil {
			reply = mustJSON(t, obj)
		}
		files[filepath.Join(fake, fmt.Sprintf("get-%d", n))] = reply
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil { //nolint:gosec // The fake tools must be executable.
			t.Fatal(err)
		}
	}

	output, err := runScript(t, []string{"FAKE=" + fake, "PATH=" + bin + ":" + os.Getenv("PATH")}, "promote", filepath.Join(dir, "want.json"))
	if raw, readErr := os.ReadFile(filepath.Join(fake, "promotions")); readErr == nil { //nolint:gosec // The test wrote this path.
		promotions = strings.Fields(string(raw))
	}
	if raw, readErr := os.ReadFile(filepath.Join(fake, "gets")); readErr == nil { //nolint:gosec // The test wrote this path.
		gets = atoi(t, strings.TrimSpace(string(raw)))
	}
	calls, _ := os.ReadFile(filepath.Join(fake, "calls")) //nolint:gosec // The test wrote this path.
	wantGet := "-n coder get codertemplatetest " + want["metadata"].(map[string]any)["name"].(string) + " -o json"
	for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if call != wantGet && call != "create --raw /apis/aggregation.coder.com/v1alpha1/namespaces/coder/codertemplates/default.docker/promote -f -" {
			t.Errorf("kubectl %s, want only %q or the promote subresource of coder/default.docker", call, wantGet)
		}
	}
	return output, err == nil, promotions, gets
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// clone deep-copies a JSON object through encoding/json.
func clone(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, v)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// inCluster returns the Git test as the API server stores it, with the given
// status and with edit applied to its spec.
func inCluster(t *testing.T, git, status map[string]any, edit func(spec map[string]any)) map[string]any {
	t.Helper()
	obj := clone(t, git)
	maps.Copy(obj["metadata"].(map[string]any), map[string]any{"generation": 1, "uid": "u1"})
	spec := obj["spec"].(map[string]any)
	if _, ok := spec["timeoutSeconds"]; !ok {
		spec["timeoutSeconds"] = 900
	}
	if edit != nil {
		edit(spec)
	}
	if status != nil {
		obj["status"] = status
	}
	return obj
}

func status(phase string) map[string]any {
	st := map[string]any{"observedGeneration": 1, "phase": phase, "reason": "R", "message": "M"}
	switch phase {
	case "Succeeded":
		st["templateVersionID"] = versionID
		st["conditions"] = []any{map[string]any{"type": "Ready", "status": "True"}}
	case "Failed":
		st["reason"], st["message"] = "AgentStartError", "agent main: startup script failed"
	}
	return st
}

func promoted(result string) string {
	return fmt.Sprintf(`{"kind":"CoderTemplateVersionPromotion","status":{"result":%q,"activeVersionID":%q}}`, result, versionID)
}

func TestPromote(t *testing.T) {
	example := render(t, filesys.MakeFsOnDisk()).test
	withParams := clone(t, example)
	withParams["spec"].(map[string]any)["parameters"] = []any{
		map[string]any{"name": "region", "value": "eu"},
		map[string]any{"name": "debug", "value": ""},
	}
	delete(withParams["spec"].(map[string]any), "timeoutSeconds")
	setTimeout := func(seconds int) func(map[string]any) {
		return func(spec map[string]any) { spec["timeoutSeconds"] = seconds }
	}
	longTest := clone(t, example)
	setTimeout(maxTestTimeoutSeconds)(longTest["spec"].(map[string]any))
	shortTest := clone(t, example)
	setTimeout(60)(shortTest["spec"].(map[string]any))
	notReady := status("Succeeded")
	notReady["conditions"] = []any{map[string]any{"type": "Ready", "status": "False"}}
	stale := status("Succeeded")
	stale["observedGeneration"] = 0
	deleting := inCluster(t, example, status("Succeeded"), nil)
	deleting["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-30T00:00:00Z"

	cases := []promoteCase{
		{
			name: "promotes the tested version after the test passed",
			gets: map[int]map[string]any{
				1: nil,
				2: inCluster(t, example, nil, nil),
				3: inCluster(t, example, stale, nil),
				4: deleting,
				5: inCluster(t, example, status("Running"), nil),
				6: inCluster(t, example, status("Succeeded"), nil),
			},
			promoteAnswer: promoted("Promoted"), wantOK: true, wantPromotions: 1, wantMinGets: 6, wantOutput: "Promoted version " + versionID,
		},
		{
			name: "waits while Succeeded has no Ready=True",
			gets: map[int]map[string]any{
				1: inCluster(t, example, notReady, nil),
				3: inCluster(t, example, status("Succeeded"), nil),
			},
			promoteAnswer: promoted("Promoted"), wantOK: true, wantPromotions: 1, wantMinGets: 3, wantOutput: "phase Succeeded, Ready=False",
		},
		{
			name:          "accepts AlreadyActive",
			gets:          map[int]map[string]any{1: inCluster(t, example, status("Succeeded"), nil)},
			promoteAnswer: promoted("AlreadyActive"), wantOK: true, wantPromotions: 1,
		},
		{
			name: "accepts the defaulted timeout, an omitted empty value, and another parameter order",
			want: withParams,
			gets: map[int]map[string]any{1: inCluster(t, withParams, status("Succeeded"), func(spec map[string]any) {
				spec["parameters"] = []any{map[string]any{"name": "debug"}, map[string]any{"name": "region", "value": "eu"}}
			})},
			promoteAnswer: promoted("Promoted"), wantOK: true, wantPromotions: 1,
		},
		{
			name: "refuses a test created first with other parameters",
			gets: map[int]map[string]any{1: inCluster(t, example, status("Succeeded"), func(spec map[string]any) {
				spec["parameters"] = []any{map[string]any{"name": "region", "value": "us"}}
			})},
			wantOutput: "refusing to promote",
		},
		{
			name: "refuses a test with another parameter value",
			want: withParams,
			gets: map[int]map[string]any{1: inCluster(t, withParams, status("Succeeded"), func(spec map[string]any) {
				spec["parameters"].([]any)[0].(map[string]any)["value"] = "us"
			})},
			wantOutput: "refusing to promote",
		},
		{
			name: "refuses a test of another version name casing",
			gets: map[int]map[string]any{1: inCluster(t, example, status("Succeeded"), func(spec map[string]any) {
				spec["version"] = map[string]any{"name": "v2.0_rc1"}
			})},
			wantOutput: "refusing to promote",
		},
		{
			name:       "refuses a test with another timeout",
			gets:       map[int]map[string]any{1: inCluster(t, example, status("Succeeded"), setTimeout(60))},
			wantOutput: "refusing to promote",
		},
		{
			name:       "refuses a still running test with another spec at once",
			gets:       map[int]map[string]any{1: inCluster(t, example, status("Running"), setTimeout(60))},
			wantOutput: "refusing to promote",
		},
		{
			name:       "fails on a failed test",
			gets:       map[int]map[string]any{1: inCluster(t, example, status("Failed"), nil)},
			wantOutput: "AgentStartError: agent main: startup script failed",
		},
		{
			name: "waits through the longest test timeout",
			want: longTest,
			gets: map[int]map[string]any{
				1: inCluster(t, longTest, status("Running"), nil),
				// 7950 s after the start: past the longest timeout.
				266: inCluster(t, longTest, status("Succeeded"), nil),
			},
			promoteAnswer: promoted("Promoted"), wantOK: true, wantPromotions: 1, wantMinGets: 266,
		},
		{
			name:        "gives up after the timeout and the start grace",
			want:        shortTest,
			gets:        map[int]map[string]any{1: inCluster(t, shortTest, status("Running"), nil)},
			wantOutput:  "did not finish within 960s",
			wantMinGets: (60 + startGraceSeconds) / pollSeconds,
		},
		{
			name:           "does not retry a failed promotion",
			gets:           map[int]map[string]any{1: inCluster(t, example, status("Succeeded"), nil)},
			promoteFails:   true,
			wantPromotions: 1, wantOutput: "Read the active version before you promote again",
		},
		{
			name:           "fails when another version is active after the promotion",
			gets:           map[int]map[string]any{1: inCluster(t, example, status("Succeeded"), nil)},
			promoteAnswer:  `{"status":{"result":"Promoted","activeVersionID":"00000000-0000-4000-8000-000000000000"}}`,
			wantPromotions: 1, wantOutput: "unexpected promotion answer",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			output, ok, promotions, gets := runPromote(t, c, example)
			if ok != c.wantOK {
				t.Errorf("success %v, want %v; output:\n%s", ok, c.wantOK, output)
			}
			if len(promotions) != c.wantPromotions {
				t.Errorf("%d promotions, want %d; output:\n%s", len(promotions), c.wantPromotions, output)
			}
			for _, body := range promotions {
				if want := fmt.Sprintf(`{"spec":{"versionID":%q}}`, versionID); body != want {
					t.Errorf("promotion body %s, want %s", body, want)
				}
			}
			if !strings.Contains(output, c.wantOutput) {
				t.Errorf("output does not contain %q:\n%s", c.wantOutput, output)
			}
			if gets < c.wantMinGets {
				t.Errorf("%d gets, want at least %d", gets, c.wantMinGets)
			}
		})
	}
}
