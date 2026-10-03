// Package gitops_test checks the GitOps health rules for CoderTemplateTest.
// The rules are data files, so this package has no production code.
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

const (
	luaPath  = "argocd-health-codertemplatetest.lua"
	fluxPath = "flux-healthcheck-codertemplatetest.yaml"
	docPath  = "../../docs/how-to/gitops.md"
)

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
		{"no status", testObject(nil), "Progressing", "Waiting for the controller", "Unknown"},
		{"no observedGeneration", testObject(noGeneration), "Progressing", "Waiting for the controller", "InProgress"},
		{"stale observedGeneration", testObject(stale), "Progressing", "Waiting for the controller", "InProgress"},
		{"pending", testObject(status("Pending", "OwnerNotEligible", notReady)), "Progressing", "OwnerNotEligible: details", "InProgress"},
		{"running", testObject(status("Running", "WaitingForBuild", notReady)), "Progressing", "WaitingForBuild: details", "InProgress"},
		{"succeeded and ready", testObject(status("Succeeded", "Succeeded", readyTrue("Succeeded"))), "Healthy", "Succeeded: details", "Current"},
		{"succeeded without ready", testObject(status("Succeeded", "Succeeded", notReady)), "Progressing", "Succeeded: details", "InProgress"},
		{"succeeded with empty conditions", testObject(status("Succeeded", "Succeeded")), "Progressing", "Succeeded: details", "InProgress"},
		{"failed", testObject(status("Failed", "BuildFailed", notReady)), "Degraded", "BuildFailed: details", "Failed"},
		// Flux has no deletion rule: it waits for pruned objects separately.
		{"being deleted", deleting, "Progressing", "Deleting the test workspace", "Current"},
	}
}

func TestArgoCDHealth(t *testing.T) {
	t.Parallel()
	script := readFile(t, luaPath)
	for _, tc := range healthCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotStatus, gotMessage := argoHealth(t, script, tc.obj)
			if gotStatus != tc.argoStatus || gotMessage != tc.argoMessage {
				t.Fatalf("got %q %q, want %q %q", gotStatus, gotMessage, tc.argoStatus, tc.argoMessage)
			}
		})
	}
}

func TestFluxHealth(t *testing.T) {
	t.Parallel()
	var checks []fluxHealthCheck
	if err := yaml.UnmarshalStrict([]byte(readFile(t, fluxPath)), &checks); err != nil {
		t.Fatalf("parse %s: %v", fluxPath, err)
	}
	if len(checks) != 1 || checks[0].APIVersion != "coder.com/v1alpha1" || checks[0].Kind != "CoderTemplateTest" {
		t.Fatalf("want one CoderTemplateTest entry, got %+v", checks)
	}
	for _, tc := range healthCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fluxHealth(t, checks[0], tc.obj); got != tc.fluxStatus {
				t.Fatalf("got %q, want %q", got, tc.fluxStatus)
			}
		})
	}
}

// TestDocCopiesRules keeps the copies in docs/how-to/gitops.md equal to the
// source files, because the docs build has no snippet include.
func TestDocCopiesRules(t *testing.T) {
	t.Parallel()
	doc := readFile(t, docPath)
	for lang, path := range map[string]string{"lua": luaPath, "yaml": fluxPath} {
		block := "```" + lang + "\n" + readFile(t, path) + "```\n"
		if !strings.Contains(doc, block) {
			t.Errorf("%s has no ```%s block equal to %s", docPath, lang, path)
		}
	}
}

// argoHealth runs the script like Argo CD: the object is the global obj, and
// the script returns a table with status and message.
func argoHealth(t *testing.T, script string, obj map[string]any) (string, string) {
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
	return lua.LVAsString(hs.RawGetString("status")), lua.LVAsString(hs.RawGetString("message"))
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
