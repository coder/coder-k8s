// Package gitops_test checks the GitOps health rules for CoderTemplateTest,
// CoderMCPServer, CoderAIProvider, CoderWorkspacePool and
// CoderWorkspaceClaim. The rules are data files, so this package has no
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
	{"CoderAIProvider", "argocd-health-coderaiprovider.lua", "flux-healthcheck-coderaiprovider.yaml", "../../docs/how-to/gitops.md", aiProviderCases},
	{"CoderWorkspacePool", "argocd-health-coderworkspacepool.lua", "flux-healthcheck-coderworkspacepool.yaml", "../../docs/how-to/gitops.md", workspacePoolCases},
	{"CoderWorkspaceClaim", "argocd-health-coderworkspaceclaim.lua", "flux-healthcheck-coderworkspaceclaim.yaml", "../../docs/how-to/gitops.md", workspaceClaimCases},
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

// readyReconcilingStalled returns the three conditions that the CoderMCPServer,
// CoderAIProvider, CoderWorkspacePool and CoderWorkspaceClaim controllers
// always write together, with one reason and message: ready, reconciling
// (neither ready nor stalled), or stalled.
func readyReconcilingStalled(state, reason string) map[string]any {
	ready, reconciling, stalled := "False", "True", "False"
	switch state {
	case "ready":
		ready, reconciling = "True", "False"
	case "stalled":
		reconciling, stalled = "False", "True"
	case "reconciling":
	default:
		panic("assertion failed: unknown condition fixture state " + state)
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
	stale := readyReconcilingStalled("ready", "Applied")
	stale["observedGeneration"] = int64(1)
	noConditions := map[string]any{"observedGeneration": int64(2)}
	// An update interrupted before Coder committed it, as stored by
	// controllers before the #415 fix: only the top-level generation moved.
	interrupted := readyReconcilingStalled("ready", "Applied")
	interrupted["appliedGeneration"] = int64(1)
	for _, c := range interrupted["conditions"].([]any) {
		c.(map[string]any)["observedGeneration"] = int64(1)
	}
	deleting := mcpServerObject(readyReconcilingStalled("ready", "Applied"))
	deleting["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-14T10:00:00Z"
	deletingWait := mcpServerObject(readyReconcilingStalled("reconciling", "CoderUnavailable"))
	deletingWait["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-14T10:00:00Z"
	return []healthCase{
		{"no status", mcpServerObject(nil), "Progressing", "Waiting for the controller", "Unknown", ""},
		{"stale observedGeneration", mcpServerObject(stale), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"no conditions", mcpServerObject(noConditions), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"applied", mcpServerObject(readyReconcilingStalled("ready", "Applied")), "Healthy", "Applied: details", "Current", ""},
		{"stale conditions", mcpServerObject(interrupted), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"control plane not ready", mcpServerObject(readyReconcilingStalled("reconciling", "ControlPlaneNotReady")), "Progressing", "ControlPlaneNotReady: details", "InProgress", ""},
		{"Coder unavailable", mcpServerObject(readyReconcilingStalled("reconciling", "CoderUnavailable")), "Progressing", "CoderUnavailable: details", "InProgress", ""},
		{"group not found", mcpServerObject(readyReconcilingStalled("stalled", "GroupNotFound")), "Degraded", "GroupNotFound: details", "Failed", ""},
		{"not allowed", mcpServerObject(readyReconcilingStalled("stalled", "MCPServersNotAllowed")), "Degraded", "MCPServersNotAllowed: details", "Failed", ""},
		{"disabled externally", mcpServerObject(readyReconcilingStalled("stalled", "DisabledExternally")), "Degraded", "DisabledExternally: details", "Failed", ""},
		{"credential not opted in", mcpServerObject(readyReconcilingStalled("stalled", "CredentialNotOptedIn")), "Degraded", "CredentialNotOptedIn: details", "Failed", ""},
		{"slug conflict", mcpServerObject(readyReconcilingStalled("stalled", "SlugConflict")), "Degraded", "SlugConflict: details", "Failed", ""},
		// Flux has no deletion rule: it waits for pruned objects separately.
		{"being deleted", deleting, "Progressing", "Deleting the Coder MCP server config", "Current", "Deleting the Coder MCP server config"},
		{"being deleted while Coder is down", deletingWait, "Progressing", "Deleting the Coder MCP server config", "InProgress", "Deleting the Coder MCP server config"},
	}
}

// aiProviderObject returns a CoderAIProvider at generation 2 with the given
// status.
func aiProviderObject(status map[string]any) map[string]any {
	obj := map[string]any{
		"apiVersion": "coder.com/v1alpha1",
		"kind":       "CoderAIProvider",
		"metadata":   map[string]any{"name": "openai", "namespace": "coder", "generation": int64(2)},
		"spec":       map[string]any{"type": "openai", "baseURL": "https://api.openai.com/v1"},
	}
	if status != nil {
		obj["status"] = status
	}
	return obj
}

func aiProviderCases() []healthCase {
	stale := readyReconcilingStalled("ready", "Applied")
	stale["observedGeneration"] = int64(1)
	noConditions := map[string]any{"observedGeneration": int64(2)}
	// An update interrupted before Coder committed it, as stored by
	// controllers before the #321 fix: only the top-level generation moved.
	interrupted := readyReconcilingStalled("ready", "Applied")
	interrupted["appliedGeneration"] = int64(1)
	for _, c := range interrupted["conditions"].([]any) {
		c.(map[string]any)["observedGeneration"] = int64(1)
	}
	deleting := aiProviderObject(readyReconcilingStalled("ready", "Applied"))
	deleting["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-30T10:00:00Z"
	deletingWait := aiProviderObject(readyReconcilingStalled("reconciling", "CoderUnavailable"))
	deletingWait["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-30T10:00:00Z"
	return []healthCase{
		{"no status", aiProviderObject(nil), "Progressing", "Waiting for the controller", "Unknown", ""},
		{"stale observedGeneration", aiProviderObject(stale), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"no conditions", aiProviderObject(noConditions), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"applied", aiProviderObject(readyReconcilingStalled("ready", "Applied")), "Healthy", "Applied: details", "Current", ""},
		{"stale conditions", aiProviderObject(interrupted), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"control plane not ready", aiProviderObject(readyReconcilingStalled("reconciling", "ControlPlaneNotReady")), "Progressing", "ControlPlaneNotReady: details", "InProgress", ""},
		{"confirming a create", aiProviderObject(readyReconcilingStalled("reconciling", "ConfirmingCreate")), "Progressing", "ConfirmingCreate: details", "InProgress", ""},
		// A disable that failed retries with the credential reason.
		{"disable retrying", aiProviderObject(readyReconcilingStalled("reconciling", "CredentialNotOptedIn")), "Progressing", "CredentialNotOptedIn: details", "InProgress", ""},
		{"credential kill switch", aiProviderObject(readyReconcilingStalled("stalled", "CredentialNotOptedIn")), "Degraded", "CredentialNotOptedIn: details", "Failed", ""},
		{"name conflict", aiProviderObject(readyReconcilingStalled("stalled", "NameConflict")), "Degraded", "NameConflict: details", "Failed", ""},
		{"name reserved by env", aiProviderObject(readyReconcilingStalled("stalled", "NameReservedByEnv")), "Degraded", "NameReservedByEnv: details", "Failed", ""},
		{"disabled externally", aiProviderObject(readyReconcilingStalled("stalled", "DisabledExternally")), "Degraded", "DisabledExternally: details", "Failed", ""},
		// Flux has no deletion rule: it waits for pruned objects separately.
		{"being deleted", deleting, "Progressing", "Deleting the Coder AI provider", "Current", "Deleting the Coder AI provider"},
		{"being deleted while Coder is down", deletingWait, "Progressing", "Deleting the Coder AI provider", "InProgress", "Deleting the Coder AI provider"},
	}
}

// workspacePoolObject returns a CoderWorkspacePool at generation 2 with the
// given status.
func workspacePoolObject(status map[string]any) map[string]any {
	obj := map[string]any{
		"apiVersion": "coder.com/v1alpha1",
		"kind":       "CoderWorkspacePool",
		"metadata":   map[string]any{"name": "ci-runners", "namespace": "coder", "generation": int64(2)},
		"spec":       map[string]any{"template": "coder.ci-runner", "replicas": int64(3)},
	}
	if status != nil {
		obj["status"] = status
	}
	return obj
}

func workspacePoolCases() []healthCase {
	// A /scale write bumps the generation, so the pool is stale until the
	// controller counts its members for the new replicas.
	rescaled := readyReconcilingStalled("ready", "Ready")
	rescaled["observedGeneration"] = int64(1)
	noConditions := map[string]any{"observedGeneration": int64(2)}
	staleConditions := readyReconcilingStalled("ready", "Ready")
	for _, c := range staleConditions["conditions"].([]any) {
		c.(map[string]any)["observedGeneration"] = int64(1)
	}
	deleting := workspacePoolObject(readyReconcilingStalled("ready", "Ready"))
	deleting["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-01T10:00:00Z"
	deletingWait := workspacePoolObject(readyReconcilingStalled("reconciling", "WaitingForMembers"))
	deletingWait["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-01T10:00:00Z"
	return []healthCase{
		{"no status", workspacePoolObject(nil), "Progressing", "Waiting for the controller", "Unknown", ""},
		{"rescaled", workspacePoolObject(rescaled), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"no conditions", workspacePoolObject(noConditions), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"stale conditions", workspacePoolObject(staleConditions), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"ready", workspacePoolObject(readyReconcilingStalled("ready", "Ready")), "Healthy", "Ready: details", "Current", ""},
		{"waiting for members", workspacePoolObject(readyReconcilingStalled("reconciling", "WaitingForMembers")), "Progressing", "WaitingForMembers: details", "InProgress", ""},
		{"owner not allowed", workspacePoolObject(readyReconcilingStalled("reconciling", "OwnerNotAllowed")), "Progressing", "OwnerNotAllowed: details", "InProgress", ""},
		{"owner cap", workspacePoolObject(readyReconcilingStalled("reconciling", "OwnerWorkspaceLimit")), "Progressing", "OwnerWorkspaceLimit: details", "InProgress", ""},
		{"failure limit", workspacePoolObject(readyReconcilingStalled("stalled", "MemberFailureLimit")), "Degraded", "MemberFailureLimit: details", "Failed", ""},
		{"version archived", workspacePoolObject(readyReconcilingStalled("stalled", "TemplateVersionArchived")), "Degraded", "TemplateVersionArchived: details", "Failed", ""},
		// Flux has no deletion rule: it waits for pruned objects separately.
		{"being deleted", deleting, "Progressing", "Deleting the workspace pool", "Current", "Deleting the workspace pool"},
		{"being deleted while members build", deletingWait, "Progressing", "Deleting the workspace pool", "InProgress", "Deleting the workspace pool"},
	}
}

// workspaceClaimObject returns a CoderWorkspaceClaim at generation 2 with the
// given status.
func workspaceClaimObject(status map[string]any) map[string]any {
	obj := map[string]any{
		"apiVersion": "coder.com/v1alpha1",
		"kind":       "CoderWorkspaceClaim",
		"metadata":   map[string]any{"name": "build-1234", "namespace": "coder", "generation": int64(2)},
		"spec":       map[string]any{"poolName": "ci-runners"},
	}
	if status != nil {
		obj["status"] = status
	}
	return obj
}

// claimStatus returns a claim status in phase with the conditions of state.
// A claim status has no observedGeneration: only its conditions carry one.
func claimStatus(phase, state, reason string) map[string]any {
	st := readyReconcilingStalled(state, reason)
	delete(st, "observedGeneration")
	st["phase"] = phase
	return st
}

func workspaceClaimCases() []healthCase {
	staleConditions := claimStatus("Bound", "ready", "WorkspaceReady")
	for _, c := range staleConditions["conditions"].([]any) {
		c.(map[string]any)["observedGeneration"] = int64(1)
	}
	deleting := workspaceClaimObject(claimStatus("Bound", "ready", "WorkspaceReady"))
	deleting["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-01T10:00:00Z"
	deletingWait := workspaceClaimObject(claimStatus("Pending", "reconciling", "WaitingForReadyWorkspace"))
	deletingWait["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-01T10:00:00Z"
	return []healthCase{
		{"no status", workspaceClaimObject(nil), "Progressing", "Waiting for the controller", "Unknown", ""},
		{"no conditions", workspaceClaimObject(map[string]any{"phase": "Pending"}), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"stale conditions", workspaceClaimObject(staleConditions), "Progressing", "Waiting for the controller", "InProgress", ""},
		{"waiting for a workspace", workspaceClaimObject(claimStatus("Pending", "reconciling", "WaitingForReadyWorkspace")), "Progressing", "WaitingForReadyWorkspace: details", "InProgress", ""},
		{"pool not found", workspaceClaimObject(claimStatus("Pending", "reconciling", "PoolNotFound")), "Progressing", "PoolNotFound: details", "InProgress", ""},
		{"bound and ready", workspaceClaimObject(claimStatus("Bound", "ready", "WorkspaceReady")), "Healthy", "WorkspaceReady: details", "Current", ""},
		{"bound, agent not ready", workspaceClaimObject(claimStatus("Bound", "reconciling", "AgentNotReady")), "Progressing", "AgentNotReady: details", "InProgress", ""},
		{"member deleted", workspaceClaimObject(claimStatus("Lost", "stalled", "MemberDeleted")), "Degraded", "MemberDeleted: details", "Failed", ""},
		{"workspace deleted", workspaceClaimObject(claimStatus("Lost", "stalled", "WorkspaceDeleted")), "Degraded", "WorkspaceDeleted: details", "Failed", ""},
		{"idle timeout", workspaceClaimObject(claimStatus("Lost", "stalled", "IdleTimeout")), "Degraded", "IdleTimeout: details", "Failed", ""},
		// The controller writes Lost and Stalled=True together. Each one alone
		// still counts.
		{"lost without conditions", workspaceClaimObject(map[string]any{"phase": "Lost"}), "Degraded", "Waiting for the controller", "Failed", ""},
		{"stalled while bound", workspaceClaimObject(claimStatus("Bound", "stalled", "WorkspaceDeleted")), "Degraded", "WorkspaceDeleted: details", "Failed", ""},
		// Flux has no deletion rule: it waits for pruned objects separately.
		{"being deleted", deleting, "Progressing", "Deleting the workspace claim", "Current", "Deleting the workspace claim"},
		{"being deleted while waiting", deletingWait, "Progressing", "Deleting the workspace claim", "InProgress", "Deleting the workspace claim"},
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
