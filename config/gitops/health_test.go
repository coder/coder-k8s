// Package gitops_test checks the GitOps health rules for CoderTemplateTest
// and CoderMCPServer. The rules are data files, so this package has no
// production code.
package gitops_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	lua "github.com/yuin/gopher-lua"
	"sigs.k8s.io/yaml"
)

// ruleSet is the Argo CD script and the Flux entry of one kind, the doc page
// that copies them, and the cases that both must pass.
type ruleSet struct {
	kind     string
	luaPath  string
	fluxPath string
	docPath  string
	cases    func() []healthCase
}

var ruleSets = []ruleSet{
	{"CoderTemplateTest", "argocd-health-codertemplatetest.lua", "flux-healthcheck-codertemplatetest.yaml", "../../docs/how-to/gitops.md", healthCases},
	{"CoderMCPServer", "argocd-health-codermcpserver.lua", "flux-healthcheck-codermcpserver.yaml", "../../docs/how-to/manage-mcp-servers.md", mcpServerCases},
}

// fluxHealthCheck is one entry of a Flux Kustomization spec.healthCheckExprs.
type fluxHealthCheck struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	InProgress string `json:"inProgress"`
	Failed     string `json:"failed"`
	Current    string `json:"current"`
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // Tests read fixed files of this repository.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readyTrue(reason string) map[string]any {
	return map[string]any{"type": "Ready", "status": "True", "reason": reason}
}

// testObject returns a CoderTemplateTest at generation 2 with the given status.
func testObject(status map[string]any) map[string]any {
	obj := map[string]any{
		"apiVersion": "coder.com/v1alpha1",
		"kind":       "CoderTemplateTest",
		"metadata":   map[string]any{"name": "v1-2-3", "namespace": "templates", "generation": int64(2)},
		"spec":       map[string]any{"templateName": "docker"},
	}
	if status != nil {
		obj["status"] = status
	}
	return obj
}

func status(phase, reason string, conditions ...any) map[string]any {
	return map[string]any{
		"observedGeneration": int64(2),
		"phase":              phase,
		"reason":             reason,
		"message":            "details",
		"conditions":         conditions,
	}
}

type healthCase struct {
	name        string
	obj         map[string]any
	argoStatus  string
	argoMessage string
	fluxStatus  string
	// argoDeletion is the deletionMessage, which Argo CD shows instead of
	// the message while the object is being deleted.
	argoDeletion string
}

func healthCases() []healthCase {
	stale := status("Succeeded", "Succeeded", readyTrue("Succeeded"))
	stale["observedGeneration"] = int64(1)
	noGeneration := status("Running", "WaitingForBuild")
	delete(noGeneration, "observedGeneration")
	deleting := testObject(status("Succeeded", "Succeeded", readyTrue("Succeeded")))
	deleting["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-03T10:00:00Z"
	notReady := map[string]any{"type": "Ready", "status": "False", "reason": "WaitingForBuild"}
	return []healthCase{
		// Flux reports a CEL error, which kstatus turns into Unknown: the
		// wait continues, like InProgress.
		{"no status", testObject(nil), "Progressing", "Waiting for the controller", "Unknown", ""},
		{"no observedGeneration", testObject(noGeneration), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"stale observedGeneration", testObject(stale), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"pending", testObject(status("Pending", "OwnerNotEligible", notReady)), "Progressing", "OwnerNotEligible: details", "InProgress", ""},
		{"running", testObject(status("Running", "WaitingForBuild", notReady)), "Progressing", "WaitingForBuild: details", "InProgress", ""},
		{"succeeded and ready", testObject(status("Succeeded", "Succeeded", readyTrue("Succeeded"))), "Healthy", "Succeeded: details", "Current", ""},
		{"succeeded without ready", testObject(status("Succeeded", "Succeeded", notReady)), "Progressing", "Succeeded: details", "InProgress", ""},
		{"succeeded with empty conditions", testObject(status("Succeeded", "Succeeded")), "Progressing", "Succeeded: details", "InProgress", ""},
		{"failed", testObject(status("Failed", "BuildFailed", notReady)), "Degraded", "BuildFailed: details", "Failed", ""},
		// Flux has no deletion rule: it waits for pruned objects separately.
		{"being deleted", deleting, "Progressing", "Deleting the test workspace", "Current", "Deleting the test workspace"},
	}
}

// mcpServerObject returns a CoderMCPServer at generation 2 with the given
// status.
func mcpServerObject(status map[string]any) map[string]any {
	obj := map[string]any{
		"apiVersion": "coder.com/v1alpha1",
		"kind":       "CoderMCPServer",
		"metadata":   map[string]any{"name": "github", "namespace": "coder", "generation": int64(2)},
		"spec":       map[string]any{"organization": "coder", "slug": "github"},
	}
	if status != nil {
		obj["status"] = status
	}
	return obj
}

// mcpServerStatus returns the three conditions that the controller always
// writes together, with one reason and message: ready, reconciling (neither
// ready nor stalled), or stalled.
func mcpServerStatus(state, reason string) map[string]any {
	ready, reconciling, stalled := "False", "True", "False"
	switch state {
	case "ready":
		ready, reconciling = "True", "False"
	case "stalled":
		reconciling, stalled = "False", "True"
	case "reconciling":
	default:
		panic("assertion failed: unknown MCP server fixture state " + state)
	}
	condition := func(conditionType, status string) any {
		return map[string]any{"type": conditionType, "status": status, "reason": reason, "message": "details", "observedGeneration": int64(2)}
	}
	return map[string]any{
		"observedGeneration": int64(2),
		"conditions":         []any{condition("Ready", ready), condition("Reconciling", reconciling), condition("Stalled", stalled)},
	}
}

func mcpServerCases() []healthCase {
	stale := mcpServerStatus("ready", "Applied")
	stale["observedGeneration"] = int64(1)
	noConditions := map[string]any{"observedGeneration": int64(2)}
	deleting := mcpServerObject(mcpServerStatus("ready", "Applied"))
	deleting["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-14T10:00:00Z"
	deletingWait := mcpServerObject(mcpServerStatus("reconciling", "CoderUnavailable"))
	deletingWait["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-14T10:00:00Z"
	return []healthCase{
		{"no status", mcpServerObject(nil), "Progressing", "Waiting for the controller", "Unknown", ""},
		{"stale observedGeneration", mcpServerObject(stale), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"no conditions", mcpServerObject(noConditions), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"applied", mcpServerObject(mcpServerStatus("ready", "Applied")), "Healthy", "Applied: details", "Current", ""},
		{"control plane not ready", mcpServerObject(mcpServerStatus("reconciling", "ControlPlaneNotReady")), "Progressing", "ControlPlaneNotReady: details", "InProgress", ""},
		{"Coder unavailable", mcpServerObject(mcpServerStatus("reconciling", "CoderUnavailable")), "Progressing", "CoderUnavailable: details", "InProgress", ""},
		{"group not found", mcpServerObject(mcpServerStatus("stalled", "GroupNotFound")), "Degraded", "GroupNotFound: details", "Failed", ""},
		{"not allowed", mcpServerObject(mcpServerStatus("stalled", "MCPServersNotAllowed")), "Degraded", "MCPServersNotAllowed: details", "Failed", ""},
		{"disabled externally", mcpServerObject(mcpServerStatus("stalled", "DisabledExternally")), "Degraded", "DisabledExternally: details", "Failed", ""},
		{"credential not opted in", mcpServerObject(mcpServerStatus("stalled", "CredentialNotOptedIn")), "Degraded", "CredentialNotOptedIn: details", "Failed", ""},
		{"slug conflict", mcpServerObject(mcpServerStatus("stalled", "SlugConflict")), "Degraded", "SlugConflict: details", "Failed", ""},
		// Flux has no deletion rule: it waits for pruned objects separately.
		{"being deleted", deleting, "Progressing", "Deleting the Coder MCP server config", "Current", "Deleting the Coder MCP server config"},
		{"being deleted while Coder is down", deletingWait, "Progressing", "Deleting the Coder MCP server config", "InProgress", "Deleting the Coder MCP server config"},
	}
}

func TestArgoCDHealth(t *testing.T) {
	t.Parallel()
	for _, rules := range ruleSets {
		script := readFile(t, rules.luaPath)
		for _, tc := range rules.cases() {
			t.Run(rules.kind+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				gotStatus, gotMessage, gotDeletion := argoHealth(t, script, tc.obj)
				if gotStatus != tc.argoStatus || gotMessage != tc.argoMessage || gotDeletion != tc.argoDeletion {
					t.Fatalf("got %q %q %q, want %q %q %q", gotStatus, gotMessage, gotDeletion, tc.argoStatus, tc.argoMessage, tc.argoDeletion)
				}
			})
		}
	}
}

func TestFluxHealth(t *testing.T) {
	t.Parallel()
	for _, rules := range ruleSets {
		var checks []fluxHealthCheck
		if err := yaml.UnmarshalStrict([]byte(readFile(t, rules.fluxPath)), &checks); err != nil {
			t.Fatalf("parse %s: %v", rules.fluxPath, err)
		}
		if len(checks) != 1 || checks[0].APIVersion != "coder.com/v1alpha1" || checks[0].Kind != rules.kind {
			t.Fatalf("want one %s entry, got %+v", rules.kind, checks)
		}
		for _, tc := range rules.cases() {
			t.Run(rules.kind+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				if got := fluxHealth(t, checks[0], tc.obj); got != tc.fluxStatus {
					t.Fatalf("got %q, want %q", got, tc.fluxStatus)
				}
			})
		}
	}
}

// TestDocCopiesRules keeps the copies in the docs equal to the source files,
// because the docs build has no snippet include.
func TestDocCopiesRules(t *testing.T) {
	t.Parallel()
	for _, rules := range ruleSets {
		doc := readFile(t, rules.docPath)
		for lang, path := range map[string]string{"lua": rules.luaPath, "yaml": rules.fluxPath} {
			block := "```" + lang + "\n" + readFile(t, path) + "```\n"
			if !strings.Contains(doc, block) {
				t.Errorf("%s has no ```%s block equal to %s", rules.docPath, lang, path)
			}
		}
	}
}

// argoHealth runs the script like Argo CD: the object is the global obj, and
// the script returns a table with status, message, and deletionMessage.
func argoHealth(t *testing.T, script string, obj map[string]any) (string, string, string) {
	t.Helper()
	l := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer l.Close()
	for _, lib := range []struct {
		name string
		open lua.LGFunction
	}{{lua.BaseLibName, lua.OpenBase}, {lua.TabLibName, lua.OpenTable}, {lua.StringLibName, lua.OpenString}, {lua.MathLibName, lua.OpenMath}} {
		l.Push(l.NewFunction(lib.open))
		l.Push(lua.LString(lib.name))
		l.Call(1, 0)
	}
	l.SetGlobal("obj", toLua(t, l, obj))
	if err := l.DoString(script); err != nil {
		t.Fatalf("run health script: %v", err)
	}
	hs, ok := l.Get(-1).(*lua.LTable)
	if !ok {
		t.Fatalf("health script returned %s, want a table", l.Get(-1).Type())
	}
	return lua.LVAsString(hs.RawGetString("status")), lua.LVAsString(hs.RawGetString("message")), lua.LVAsString(hs.RawGetString("deletionMessage"))
}

func toLua(t *testing.T, l *lua.LState, value any) lua.LValue {
	t.Helper()
	switch v := value.(type) {
	case nil:
		return lua.LNil
	case string:
		return lua.LString(v)
	case bool:
		return lua.LBool(v)
	case int64:
		return lua.LNumber(v)
	case map[string]any:
		table := l.NewTable()
		for key, item := range v {
			table.RawSetString(key, toLua(t, l, item))
		}
		return table
	case []any:
		table := l.NewTable()
		for _, item := range v {
			table.Append(toLua(t, l, item))
		}
		return table
	default:
		t.Fatalf("assertion failed: unsupported fixture value %T", value)
		return lua.LNil
	}
}

// fluxHealth mirrors StatusEvaluator.Evaluate in fluxcd/pkg/runtime/cel: a
// stale status.observedGeneration is InProgress, then inProgress, failed, and
// current run in that order, and the first true one wins. An evaluation error
// becomes Unknown, as in the kstatus generic status reader.
func fluxHealth(t *testing.T, check fluxHealthCheck, obj map[string]any) string {
	t.Helper()
	if observed, ok := nested(obj, "status", "observedGeneration").(int64); ok {
		if generation, ok := nested(obj, "metadata", "generation").(int64); ok && observed != generation {
			return "InProgress"
		}
	}
	for _, rule := range []struct{ expr, status string }{
		{check.InProgress, "InProgress"},
		{check.Failed, "Failed"},
		{check.Current, "Current"},
	} {
		result, err := evalFluxCEL(t, rule.expr, obj)
		if err != nil {
			return "Unknown"
		}
		if result {
			return rule.status
		}
	}
	return "InProgress"
}

// evalFluxCEL parses without type checks and uses the environment options of
// NewExpression in fluxcd/pkg/runtime/cel.
func evalFluxCEL(t *testing.T, expr string, obj map[string]any) (bool, error) {
	t.Helper()
	env, err := cel.NewEnv(
		cel.HomogeneousAggregateLiterals(),
		cel.EagerlyValidateDeclarations(true),
		cel.DefaultUTCTimeZone(true),
		cel.CrossTypeNumericComparisons(true),
		cel.OptionalTypes(),
		ext.Strings(),
		ext.Sets(),
		ext.Encoders(),
	)
	if err != nil {
		t.Fatalf("create CEL environment: %v", err)
	}
	ast, issues := env.Parse(expr)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("parse %q: %v", expr, issues.Err())
	}
	program, err := env.Program(ast, cel.EvalOptions(cel.OptOptimize))
	if err != nil {
		t.Fatalf("program %q: %v", expr, err)
	}
	out, _, err := program.ContextEval(context.Background(), obj)
	if err != nil {
		return false, err
	}
	result, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression %q returned %T, want bool", expr, out.Value())
	}
	return result, nil
}

func nested(obj map[string]any, fields ...string) any {
	var value any = obj
	for _, field := range fields {
		m, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = m[field]
	}
	return value
}
