package controller_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// poolMemberEnv adds to templateTestEnv a control plane that allows "tester"
// with "default.docker", and the pool "runners" whose status.resolved pins
// version v1 without parameters.
type poolMemberEnv struct {
	*templateTestEnv
	pool *coderv1alpha1.CoderWorkspacePool
}

func newPoolMemberEnv(t *testing.T) *poolMemberEnv {
	t.Helper()
	e := &poolMemberEnv{templateTestEnv: newTemplateTestEnv(t)}
	e.allowOwner(t, e.tester.String(), "default.docker")
	e.pool = &coderv1alpha1.CoderWorkspacePool{
		ObjectMeta: metav1.ObjectMeta{Name: "runners", Namespace: e.ns},
		Spec: coderv1alpha1.CoderWorkspacePoolSpec{
			ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"},
			OwnerUserID:     e.tester.String(), Template: "default.docker",
			Version: coderv1alpha1.CoderWorkspacePoolVersion{Name: "v1"},
		},
	}
	require.NoError(t, k8sClient.Create(e.ctx, e.pool))
	e.resolve(t, nil)
	return e
}

// allowOwner sets the control plane's owners list to one owner, or clears it.
func (e *poolMemberEnv) allowOwner(t *testing.T, ownerID string, templates ...string) {
	t.Helper()
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Spec.WorkspacePools = nil
	if ownerID != "" {
		cp.Spec.WorkspacePools = &coderv1alpha1.WorkspacePoolsSpec{Owners: []coderv1alpha1.WorkspacePoolOwner{{UserID: ownerID, Templates: templates}}}
	}
	require.NoError(t, k8sClient.Update(e.ctx, cp))
}

// setPoolSelectors sets the pool's spec.presetName and spec.parameters.
func (e *poolMemberEnv) setPoolSelectors(t *testing.T, presetName string, params ...coderv1alpha1.CoderWorkspacePoolParameter) {
	t.Helper()
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}, e.pool))
	e.pool.Spec.PresetName, e.pool.Spec.Parameters = presetName, params
	require.NoError(t, k8sClient.Update(e.ctx, e.pool))
}

// resolve writes the pool's status.resolved: v1 without parameters, then edit.
func (e *poolMemberEnv) resolve(t *testing.T, edit func(*coderv1alpha1.CoderWorkspacePoolResolved)) {
	t.Helper()
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}, e.pool))
	e.pool.Status.Resolved = &coderv1alpha1.CoderWorkspacePoolResolved{
		OrganizationID: e.orgID.String(), TemplateID: e.tplID.String(), TemplateVersionID: e.v1.String(), TemplateVersionName: "v1",
		OwnerID: e.tester.String(), ParametersHash: parametersHash(),
	}
	if edit != nil {
		edit(e.pool.Status.Resolved)
	}
	require.NoError(t, k8sClient.Status().Update(e.ctx, e.pool))
}

// parametersHash computes the pool's parameters hash on its own, as a second
// implementation: SHA-256 of length-prefixed names and values, sorted by name.
func parametersHash(params ...coderv1alpha1.CoderWorkspacePoolMemberParameter) string {
	sorted := slices.Clone(params)
	slices.SortFunc(sorted, func(a, b coderv1alpha1.CoderWorkspacePoolMemberParameter) int { return strings.Compare(a.Name, b.Name) })
	var b strings.Builder
	for _, p := range sorted {
		fmt.Fprintf(&b, "%d:%s%d:%s", len(p.Name), p.Name, len(p.Value), p.Value)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// createMember creates a member as the pool would: controlled by the pool,
// labeled with its UID, with the pool's resolved inputs. edit runs last.
func (e *poolMemberEnv) createMember(t *testing.T, edit func(*coderv1alpha1.CoderWorkspacePoolMember)) types.NamespacedName {
	t.Helper()
	resolved := e.pool.Status.Resolved
	member := &coderv1alpha1.CoderWorkspacePoolMember{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: e.pool.Name + "-", Namespace: e.ns,
			Labels: map[string]string{coderv1alpha1.CoderWorkspacePoolSelectorLabel: string(e.pool.UID)},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: coderv1alpha1.GroupVersion.String(), Kind: "CoderWorkspacePool",
				Name: e.pool.Name, UID: e.pool.UID, Controller: ptr.To(true),
			}},
		},
		Spec: coderv1alpha1.CoderWorkspacePoolMemberSpec{
			ControlPlaneRef: e.pool.Spec.ControlPlaneRef, PoolName: e.pool.Name,
			OwnerID: resolved.OwnerID, OrganizationID: resolved.OrganizationID, TemplateID: resolved.TemplateID,
			TemplateVersionID: resolved.TemplateVersionID, PresetID: resolved.PresetID, ParametersHash: resolved.ParametersHash,
			ReadyTimeoutSeconds: 900,
		},
	}
	if edit != nil {
		edit(member)
	}
	require.NoError(t, k8sClient.Create(e.ctx, member))
	return types.NamespacedName{Namespace: member.Namespace, Name: member.Name}
}

// reconcileMember runs n reconciles, each with a new reconciler as after a
// restart, and returns the stored member (nil once it is gone).
func (e *poolMemberEnv) reconcileMember(t *testing.T, key types.NamespacedName, n int) *coderv1alpha1.CoderWorkspacePoolMember {
	t.Helper()
	for range n {
		r := &controller.CoderWorkspacePoolMemberReconciler{Client: k8sClient, Scheme: scheme, Clock: e.clock}
		var err error
		e.lastStep, err = r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	member := &coderv1alpha1.CoderWorkspacePoolMember{}
	err := k8sClient.Get(e.ctx, key, member)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return member
}

func requireMember(t *testing.T, member *coderv1alpha1.CoderWorkspacePoolMember, phase, reason, messagePart string) {
	t.Helper()
	require.Equal(t, phase, member.Status.Phase, member.Status.Message)
	require.Equal(t, reason, member.Status.Reason, member.Status.Message)
	require.Contains(t, member.Status.Message, messagePart)
	require.Equal(t, phase == coderv1alpha1.CoderWorkspacePoolMemberPhaseReady && reason != "AgentNotReady",
		meta.IsStatusConditionTrue(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionReady), "Ready=True exactly in phase Ready, unless an agent is not ready")
	require.True(t, controllerutil.ContainsFinalizer(member, coderv1alpha1.CoderWorkspacePoolMemberCleanupFinalizer))
}

// requireMemberFailed checks a failed member and its WorkspaceDeleted condition.
func requireMemberFailed(t *testing.T, member *coderv1alpha1.CoderWorkspacePoolMember, reason, messagePart string, deleted metav1.ConditionStatus) {
	t.Helper()
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed, reason, messagePart)
	condition := meta.FindStatusCondition(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted)
	require.NotNil(t, condition)
	require.Equal(t, deleted, condition.Status)
	// NotCreated exactly when no create request can have had an effect.
	require.Equal(t, condition.Reason == "NotCreated", member.Status.CreateAttemptTime == nil && member.Status.WorkspaceID == "", condition.Reason)
}

func TestPoolMemberCreatesWorkspace(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	region := coderv1alpha1.CoderWorkspacePoolMemberParameter{Name: "region", Value: "eu-west-1"}
	size := coderv1alpha1.CoderWorkspacePoolMemberParameter{Name: "size", Value: ""}
	preset := e.fake.addPreset(e.v1, "small")
	e.setPoolSelectors(t, "small", coderv1alpha1.CoderWorkspacePoolParameter{Name: "region", Value: "eu-west-1"}, coderv1alpha1.CoderWorkspacePoolParameter{Name: "size"})
	e.resolve(t, func(r *coderv1alpha1.CoderWorkspacePoolResolved) {
		r.PresetID, r.ParametersHash = preset.String(), parametersHash(size, region)
	})
	key := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) {
		m.Spec.Parameters = []coderv1alpha1.CoderWorkspacePoolMemberParameter{region, size}
	})

	member := e.reconcileMember(t, key, 1)
	require.True(t, controllerutil.ContainsFinalizer(member, coderv1alpha1.CoderWorkspacePoolMemberCleanupFinalizer))
	require.Empty(t, member.Status.Phase, "the first reconcile only adds the finalizer")
	require.Zero(t, e.fake.totalRequests(), "no Coder call before the finalizer is stored")

	member = e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "WaitingForBuild", "was created")
	require.Equal(t, "kwp-"+strings.ReplaceAll(string(member.UID), "-", "")[:28], member.Status.WorkspaceName)
	require.NotNil(t, member.Status.CreateAttemptTime)
	require.Equal(t, 5*time.Second, e.lastStep.RequeueAfter)
	ws, err := e.fake.client(t, 5*time.Second).Workspace(e.ctx, uuid.MustParse(member.Status.WorkspaceID))
	require.NoError(t, err)
	require.Equal(t, member.Status.WorkspaceName, ws.Name)
	require.Equal(t, e.tester, ws.OwnerID)
	require.Equal(t, member.Status.StartBuildID, ws.LatestBuild.ID.String())
	require.Equal(t, e.v1, ws.LatestBuild.TemplateVersionID)
	require.Equal(t, &preset, ws.LatestBuild.TemplateVersionPresetID)
	require.Equal(t, codersdk.AutomaticUpdatesNever, ws.AutomaticUpdates)
	require.ElementsMatch(t, []codersdk.WorkspaceBuildParameter{{Name: "region", Value: "eu-west-1"}, {Name: "size", Value: ""}}, e.fake.createParams(ws.ID))

	member = e.reconcileMember(t, key, 3)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "WaitingForBuild", "")
	require.Equal(t, 1, e.fake.requestCount(routeCreateWorkspace), "never a second create request")

	// A member whose workspace exists keeps its finalizer on deletion until
	// the workspace is deleted.
	require.NoError(t, k8sClient.Delete(e.ctx, member))
	require.NotNil(t, e.reconcileMember(t, key, 2))
}

// TestPoolMemberCreateResults covers the CoderTemplateTest result table and
// mutation check 2: after an uncertain result, the member never sends a
// second create request.
func TestPoolMemberCreateResults(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	cases := []struct {
		name    string
		fault   fakeFault
		phase   string
		reason  string
		message string
		// confirmed is the reason once confirming reads ran after an
		// uncertain or malformed answer.
		confirmed string
	}{
		{name: "429", fault: fakeFault{Status: 429}, phase: coderv1alpha1.CoderWorkspacePoolMemberPhasePending, reason: "CreateRetrying", message: "Coder answered 429"},
		{name: "401", fault: fakeFault{Status: 401}, phase: coderv1alpha1.CoderWorkspacePoolMemberPhasePending, reason: "CreateRetrying", message: "Coder answered 401"},
		{name: "406", fault: fakeFault{Status: 406}, phase: coderv1alpha1.CoderWorkspacePoolMemberPhasePending, reason: "TemplateVersionImporting"},
		{name: "409", fault: fakeFault{Status: 409}, phase: coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed, reason: "WorkspaceNameConflict"},
		{name: "400", fault: fakeFault{Status: 400}, phase: coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed, reason: "CreateRejected", message: "Coder answered 400: fake fault 400"},
		{name: "502 before commit", fault: fakeFault{Status: 502}, phase: coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, reason: "ConfirmingCreate", message: "Coder answered 502", confirmed: "ConfirmingCreate"},
		{name: "504 after commit", fault: fakeFault{Status: 504, AfterCommit: true}, phase: coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, reason: "ConfirmingCreate", confirmed: "WaitingForBuild"},
		{name: "connection reset", fault: fakeFault{Reset: true}, phase: coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, reason: "ConfirmingCreate", confirmed: "ConfirmingCreate"},
		{name: "201 proxy page after commit", fault: fakeFault{Status: 201, AfterCommit: true, HTML: "<html>proxy</html>"}, phase: coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, reason: "ConfirmingCreate", confirmed: "WaitingForBuild"},
		{name: "wrong workspace in the answer", fault: fakeFault{Rewrite: func(a any) any {
			ws := a.(codersdk.Workspace)
			ws.OwnerID = uuid.New()
			return ws
		}}, phase: coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, reason: "CoderAnswerMismatch", message: "Coder answered workspace owner ", confirmed: "WaitingForBuild"},
	}
	for _, tc := range cases {
		creates := e.fake.requestCount(routeCreateWorkspace)
		key := e.createMember(t, nil)
		e.reconcileMember(t, key, 1) // Finalizer.
		e.fake.failNext(routeCreateWorkspace, tc.fault)
		member := e.reconcileMember(t, key, 1)
		require.Equal(t, creates+1, e.fake.requestCount(routeCreateWorkspace), tc.name)
		require.Empty(t, member.Status.WorkspaceID, tc.name)

		switch tc.phase {
		case coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed:
			requireMemberFailed(t, member, tc.reason, tc.message, metav1.ConditionTrue)
			// A refused request had no effect, so deletion releases the member.
			require.NoError(t, k8sClient.Delete(e.ctx, member))
			require.Nil(t, e.reconcileMember(t, key, 1), tc.name)
		case coderv1alpha1.CoderWorkspacePoolMemberPhasePending:
			requireMember(t, member, tc.phase, tc.reason, tc.message)
			require.Nil(t, member.Status.CreateAttemptTime, "%s: a result without effect clears the marker", tc.name)
			if tc.name == "429" {
				require.InDelta(t, 2*time.Second, e.lastStep.RequeueAfter, float64(400*time.Millisecond), "429 backs off with jitter")
			}
			requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "WaitingForBuild", "")
			require.Equal(t, creates+2, e.fake.requestCount(routeCreateWorkspace), "%s: one more request after the retry", tc.name)
		default:
			requireMember(t, member, tc.phase, tc.reason, tc.message)
			require.NotNil(t, member.Status.CreateAttemptTime, tc.name)
			member = e.reconcileMember(t, key, 4)
			requireMember(t, member, tc.phase, tc.confirmed, "")
			require.Equal(t, tc.confirmed == "WaitingForBuild", member.Status.WorkspaceID != "", "%s: only a confirmed workspace is adopted", tc.name)
			require.Equal(t, creates+1, e.fake.requestCount(routeCreateWorkspace), "%s: never a second create request", tc.name)
		}
	}
}

// TestPoolMemberMarkerConflictSendsNoRequest is mutation check 1: the marker
// is stored before the create request.
func TestPoolMemberMarkerConflictSendsNoRequest(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := e.createMember(t, nil)
	e.reconcileMember(t, key, 1) // Finalizer.

	r := &controller.CoderWorkspacePoolMemberReconciler{Client: markerConflictClient{k8sClient}, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.True(t, apierrors.IsConflict(err), "the marker write conflict ends the reconcile: %v", err)
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace), "no create request without a stored marker")
	require.Nil(t, e.reconcileMember(t, key, 0).Status.CreateAttemptTime)
}

// TestPoolMemberClearsMarkerAfterRepeatedNoEffect: two create results
// without effect in a row each clear the stored marker, so the member
// retries instead of waiting for a workspace that does not exist.
func TestPoolMemberClearsMarkerAfterRepeatedNoEffect(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := e.createMember(t, nil)
	e.reconcileMember(t, key, 1) // Finalizer.
	for range 2 {
		e.fake.failNext(routeCreateWorkspace, fakeFault{Status: 429})
		member := e.reconcileMember(t, key, 1)
		requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhasePending, "CreateRetrying", "Coder answered 429")
		require.Nil(t, member.Status.CreateAttemptTime, "the stored marker is cleared")
	}
	requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "WaitingForBuild", "")
	require.Equal(t, 3, e.fake.requestCount(routeCreateWorkspace))
}

// TestPoolMemberChecksPresetInAnswer: a create answer without the requested
// preset is a wrong answer, so the marker stays and only reads decide.
func TestPoolMemberChecksPresetInAnswer(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	preset := e.fake.addPreset(e.v1, "small")
	e.setPoolSelectors(t, "small")
	e.resolve(t, func(r *coderv1alpha1.CoderWorkspacePoolResolved) { r.PresetID = preset.String() })
	key := e.createMember(t, nil)
	e.reconcileMember(t, key, 1) // Finalizer.
	e.fake.failNext(routeCreateWorkspace, fakeFault{Rewrite: func(a any) any {
		ws := a.(codersdk.Workspace)
		ws.LatestBuild.TemplateVersionPresetID = nil
		return ws
	}})
	member := e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "CoderAnswerMismatch", "Coder answered start build preset ")
	require.NotNil(t, member.Status.CreateAttemptTime)
	require.Empty(t, member.Status.WorkspaceID)
}

func TestPoolMemberOwnershipGate(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	for _, tc := range []struct {
		name  string
		edit  func(*coderv1alpha1.CoderWorkspacePoolMember)
		cause string
	}{
		{name: "no owner", edit: func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.OwnerReferences = nil }, cause: "does not control"},
		{name: "owner that is not the controller", edit: func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.OwnerReferences[0].Controller = nil }, cause: "does not control"},
		{name: "controlled by a claim", edit: func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.OwnerReferences[0].Kind = "CoderWorkspaceClaim" }, cause: "does not control"},
		{name: "controlled by another pool", edit: func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.OwnerReferences[0].Name = "other" }, cause: "does not control"},
		{name: "controlled by an old pool of the same name", edit: func(m *coderv1alpha1.CoderWorkspacePoolMember) {
			m.OwnerReferences[0].UID = types.UID(uuid.NewString())
		}, cause: "not the pool that created"},
	} {
		key := e.createMember(t, tc.edit)
		member := e.reconcileMember(t, key, 3)
		requireMemberFailed(t, member, "NotOwned", tc.cause, metav1.ConditionTrue)
		require.NoError(t, k8sClient.Delete(e.ctx, member))
		require.Nil(t, e.reconcileMember(t, key, 1), "%s: a member without a workspace is released at once", tc.name)
	}
	require.Zero(t, e.fake.totalRequests(), "the gate runs before any Coder call")
}

func TestPoolMemberWaitsForConsentAndOwner(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := e.createMember(t, nil)
	e.reconcileMember(t, key, 1) // Finalizer.
	requireWait := func(reason, messagePart string) {
		t.Helper()
		requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhasePending, reason, messagePart)
		require.Equal(t, 15*time.Second, e.lastStep.RequeueAfter)
	}

	e.allowOwner(t, "")
	requireWait("OwnerNotAllowed", "spec.workspacePools.owners")
	e.allowOwner(t, uuid.NewString(), "default.docker")
	requireWait("OwnerNotAllowed", e.tester.String())
	e.allowOwner(t, e.tester.String(), "default.other")
	requireWait("TemplateNotAllowed", `"default.docker"`)
	e.allowOwner(t, e.tester.String(), "Default.Docker")
	e.fake.updateUser(e.tester, func(u *codersdk.User) { u.Status = codersdk.UserStatusSuspended })
	requireWait("OwnerNotEligible", "Only active users can own workspaces that the operator creates")
	e.fake.updateUser(e.tester, func(u *codersdk.User) {
		u.Status, u.Roles = codersdk.UserStatusActive, []codersdk.SlimRole{{Name: codersdk.RoleTemplateAdmin}}
	})
	requireWait("OwnerNotEligible", "site role")
	e.fake.updateUser(e.tester, func(u *codersdk.User) { u.Roles = nil })
	e.fake.failNext(routeTemplateByName, fakeFault{Status: 503})
	requireWait("CoderUnavailable", "get template: Coder answered 503")
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace), "no create request while consent or an owner rule fails")

	// Template names match without case, as in Coder.
	requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "WaitingForBuild", "")
}

// TestTemplateTestRefusesPrebuildsOwner covers the shared owner rule: Coder's
// prebuilds system user never owns a workspace, in any spelling, and the
// controller refuses it without asking Coder.
func TestTemplateTestRefusesPrebuildsOwner(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	for _, ownerID := range []string{prebuildsUserID, strings.ToUpper(prebuildsUserID)} {
		e.setOwner(t, "coder", ownerID)
		key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
		requireTemplateTestWaiting(t, e.settle(t, key), "OwnerNotEligible", "prebuilds system user")
	}
	require.Zero(t, e.fake.requestCount(routeUser))
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace))
}

func TestPoolMemberRefusesOtherInputs(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	v2 := e.fake.addVersion(e.tplID, "v2", codersdk.ProvisionerJobSucceeded)
	otherTpl := e.fake.addTemplate(e.orgID, "other")
	otherVersion := e.fake.addVersion(otherTpl, "v1", codersdk.ProvisionerJobSucceeded)
	e.fake.addPreset(e.v1, "small")
	large := e.fake.addPreset(e.v1, "large")
	type member = coderv1alpha1.CoderWorkspacePoolMember
	for _, tc := range []struct {
		name       string
		presetName string
		params     []coderv1alpha1.CoderWorkspacePoolParameter
		edit       func(*member)
		resolve    func(*coderv1alpha1.CoderWorkspacePoolResolved)
		message    string
	}{
		{name: "other version", edit: func(m *member) { m.Spec.TemplateVersionID = v2.String() }, message: "spec.templateVersionID"},
		{name: "other owner", edit: func(m *member) { m.Spec.OwnerID = uuid.NewString() }, message: "spec.ownerID"},
		{name: "other control plane", edit: func(m *member) { m.Spec.ControlPlaneRef.Name = "other" }, message: "spec.controlPlaneRef"},
		{name: "parameters without their hash", edit: func(m *member) {
			m.Spec.Parameters = []coderv1alpha1.CoderWorkspacePoolMemberParameter{{Name: "region", Value: "us"}}
		}, message: "spec.parameters"},
		{name: "pool parameters changed before the pool resolved again", params: []coderv1alpha1.CoderWorkspacePoolParameter{{Name: "region", Value: "us"}}, message: "spec.parameters"},
		{name: "no pool label", edit: func(m *member) { m.Labels = nil }, message: "label coder.com/workspace-pool-uid"},
		// Someone with write access to the pool status points it at another
		// template. The member resolves the allowed template name again.
		{name: "pool status names another template", resolve: func(r *coderv1alpha1.CoderWorkspacePoolResolved) {
			r.TemplateID, r.TemplateVersionID = otherTpl.String(), otherVersion.String()
		}, message: "is not the pinned template"},
		// The same for the version and the preset: the member resolves the
		// pool's selectors again.
		{name: "pool status names another version", resolve: func(r *coderv1alpha1.CoderWorkspacePoolResolved) {
			r.TemplateVersionID = v2.String()
		}, message: "The pool selects version " + e.v1.String()},
		{name: "pool status names another preset", presetName: "small", resolve: func(r *coderv1alpha1.CoderWorkspacePoolResolved) {
			r.PresetID = large.String()
		}, message: `Preset "small" is `},
		{name: "pool status names a preset that the pool does not select", resolve: func(r *coderv1alpha1.CoderWorkspacePoolResolved) {
			r.PresetID = large.String()
		}, message: "selects no preset"},
	} {
		e.setPoolSelectors(t, tc.presetName, tc.params...)
		e.resolve(t, tc.resolve)
		key := e.createMember(t, tc.edit)
		requireMemberFailed(t, e.reconcileMember(t, key, 2), "InputsMismatch", tc.message, metav1.ConditionTrue)
	}
	e.resolve(t, nil)
	key := e.createMember(t, nil)
	e.pool.Status.Resolved = nil
	require.NoError(t, k8sClient.Status().Update(e.ctx, e.pool))
	requireMemberFailed(t, e.reconcileMember(t, key, 2), "InputsMismatch", "status.resolved", metav1.ConditionTrue)

	e.setPoolSelectors(t, "missing")
	e.resolve(t, func(r *coderv1alpha1.CoderWorkspacePoolResolved) { r.PresetID = uuid.NewString() })
	key = e.createMember(t, nil)
	requireMember(t, e.reconcileMember(t, key, 2), coderv1alpha1.CoderWorkspacePoolMemberPhasePending, "PresetNotFound", `has no preset "missing"`)
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace))
}

func TestPoolMemberDeadline(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := e.createMember(t, nil)
	member := e.reconcileMember(t, key, 1)
	e.clock.SetTime(member.CreationTimestamp.Add(900 * time.Second))
	requireMemberFailed(t, e.reconcileMember(t, key, 1), "DeadlineExceeded", "not ready within 900s", metav1.ConditionTrue)
	require.Zero(t, e.fake.totalRequests(), "a member past its deadline asks Coder nothing")

	// After a create request, the failed member keeps its finalizer: the
	// workspace exists until the delete steps remove it.
	e.clock.SetTime(time.Now().Truncate(time.Second))
	key = e.createMember(t, nil)
	member = e.reconcileMember(t, key, 2)
	require.NotEmpty(t, member.Status.WorkspaceID)
	e.clock.SetTime(member.CreationTimestamp.Add(901 * time.Second))
	member = e.reconcileMember(t, key, 1)
	requireMemberFailed(t, member, "DeadlineExceeded", "Last wait: WaitingForBuild", metav1.ConditionFalse)
	require.NoError(t, k8sClient.Delete(e.ctx, member))
	require.NotNil(t, e.reconcileMember(t, key, 1))
}

func TestPoolMemberNeverTakesAnotherWorkspace(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := e.createMember(t, nil)
	member := e.reconcileMember(t, key, 1)
	name := "kwp-" + strings.ReplaceAll(string(member.UID), "-", "")[:28]
	_, err := e.fake.client(t, 5*time.Second).CreateUserWorkspace(e.ctx, e.tester.String(), codersdk.CreateWorkspaceRequest{TemplateVersionID: e.v1, Name: name})
	require.NoError(t, err)

	requireMemberFailed(t, e.reconcileMember(t, key, 2), "WorkspaceNameConflict", "already has a workspace named "+name, metav1.ConditionTrue)
	require.Equal(t, 1, e.fake.requestCount(routeCreateWorkspace), "only the test's own create request")
}
