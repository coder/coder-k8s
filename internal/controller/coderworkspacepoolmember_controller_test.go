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
	require.True(t, meta.IsStatusConditionFalse(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionReady))
	require.True(t, controllerutil.ContainsFinalizer(member, coderv1alpha1.CoderWorkspacePoolMemberCleanupFinalizer))
}

// requireMemberFailed checks a failed member and its WorkspaceDeleted condition.
func requireMemberFailed(t *testing.T, member *coderv1alpha1.CoderWorkspacePoolMember, reason, messagePart string, deleted metav1.ConditionStatus) {
	t.Helper()
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed, reason, messagePart)
	condition := meta.FindStatusCondition(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted)
	require.NotNil(t, condition)
	require.Equal(t, deleted, condition.Status)
	require.Equal(t, deleted == metav1.ConditionTrue, member.Status.CreateAttemptTime == nil && member.Status.WorkspaceID == "",
		"WorkspaceDeleted=True exactly when no create request can have had an effect")
}

func TestPoolMemberChecksBeforeCreate(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := e.createMember(t, nil)

	member := e.reconcileMember(t, key, 1)
	require.True(t, controllerutil.ContainsFinalizer(member, coderv1alpha1.CoderWorkspacePoolMemberCleanupFinalizer))
	require.Empty(t, member.Status.Phase, "the first reconcile only adds the finalizer")
	require.Zero(t, e.fake.totalRequests(), "no Coder call before the finalizer is stored")

	member = e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhasePending, "CreatePending", "Every check passed")
	require.Equal(t, 15*time.Second, e.lastStep.RequeueAfter)
	require.Positive(t, e.fake.requestCount(routeOrgMember), "the owner rules ran")
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace))
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
	e.fake.failNext(routeOrgMember, fakeFault{Status: 503})
	requireWait("CoderUnavailable", "get owner membership: Coder answered 503")
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace), "no create request while consent or an owner rule fails")

	// Template names match without case, as in Coder.
	requireWait("CreatePending", "Every check passed")
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

func TestPoolMemberDeadline(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := e.createMember(t, nil)
	member := e.reconcileMember(t, key, 1)
	e.clock.SetTime(member.CreationTimestamp.Add(900 * time.Second))
	requireMemberFailed(t, e.reconcileMember(t, key, 1), "DeadlineExceeded", "not ready within 900s", metav1.ConditionTrue)
	require.Zero(t, e.fake.totalRequests(), "a member past its deadline asks Coder nothing")
}
