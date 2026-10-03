package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// templateTestEnv is one namespace with a control plane that talks to a fake
// Coder. The fake holds organization "default" with template "docker" and its
// imported version "v1". The control plane's test owner is the fake user
// "tester", a plain member of "default".
type templateTestEnv struct {
	ctx      context.Context
	fake     *fakeCoder
	clock    *clocktesting.FakePassiveClock
	ns       string
	orgID    uuid.UUID
	tplID    uuid.UUID
	v1       uuid.UUID
	tester   uuid.UUID
	lastStep ctrl.Result
}

func newTemplateTestEnv(t *testing.T) *templateTestEnv {
	t.Helper()
	ctx := context.Background()
	// Status times have second precision, so the fake clock starts on a second.
	e := &templateTestEnv{ctx: ctx, fake: newFakeCoder(t), clock: clocktesting.NewFakePassiveClock(time.Now().Truncate(time.Second))}
	e.ns = createTestNamespace(ctx, t, "ktt-ctrl")
	e.orgID = e.fake.addOrganization("default")
	e.tplID = e.fake.addTemplate(e.orgID, "docker")
	e.v1 = e.fake.addVersion(e.tplID, "v1", codersdk.ProvisionerJobSucceeded)
	e.tester = e.fake.addUser("tester", codersdk.LoginTypePassword)
	e.fake.addMember(e.orgID, e.tester)
	createTestControlPlane(ctx, t, e.ns, "coder", e.fake.server.URL)
	e.setOwner(t, "coder", e.tester.String())
	return e
}

func (e *templateTestEnv) setOwner(t *testing.T, controlPlane, ownerID string) {
	t.Helper()
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: controlPlane}, cp))
	cp.Spec.TemplateTests = &coderv1alpha1.TemplateTestsSpec{OwnerUserID: ownerID}
	if ownerID == "" {
		cp.Spec.TemplateTests = nil
	}
	require.NoError(t, k8sClient.Update(e.ctx, cp))
}

func (e *templateTestEnv) createTest(t *testing.T, template string, version coderv1alpha1.CoderTemplateTestVersion, timeoutSeconds ...int32) types.NamespacedName {
	t.Helper()
	tt := &coderv1alpha1.CoderTemplateTest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "test-", Namespace: e.ns},
		Spec: coderv1alpha1.CoderTemplateTestSpec{
			ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"},
			Template:        template, Version: version,
		},
	}
	if len(timeoutSeconds) > 0 {
		tt.Spec.TimeoutSeconds = &timeoutSeconds[0]
	}
	require.NoError(t, k8sClient.Create(e.ctx, tt))
	return types.NamespacedName{Namespace: tt.Namespace, Name: tt.Name}
}

// reconcile runs n reconciles, each with a new reconciler as after a restart,
// and returns the stored object (nil once it is gone).
func (e *templateTestEnv) reconcile(t *testing.T, key types.NamespacedName, n int) *coderv1alpha1.CoderTemplateTest {
	t.Helper()
	for range n {
		r := &controller.CoderTemplateTestReconciler{Client: k8sClient, Scheme: scheme, Clock: e.clock}
		var err error
		e.lastStep, err = r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	tt := &coderv1alpha1.CoderTemplateTest{}
	err := k8sClient.Get(e.ctx, key, tt)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return tt
}

// workspaceName is the deterministic Coder workspace name of a test.
func (e *templateTestEnv) workspaceName(t *testing.T, key types.NamespacedName) string {
	t.Helper()
	tt := &coderv1alpha1.CoderTemplateTest{}
	require.NoError(t, k8sClient.Get(e.ctx, key, tt))
	return "ktt-" + strings.ReplaceAll(string(tt.UID), "-", "")[:28]
}

// settle reconciles until a waiting test is stable and a failed test has
// released its finalizer.
func (e *templateTestEnv) settle(t *testing.T, key types.NamespacedName) *coderv1alpha1.CoderTemplateTest {
	t.Helper()
	return e.reconcile(t, key, 4)
}

func requireTemplateTestWaiting(t *testing.T, tt *coderv1alpha1.CoderTemplateTest, reason, messagePart string) {
	t.Helper()
	requireTemplateTestPhase(t, tt, coderv1alpha1.CoderTemplateTestPhasePending, reason, messagePart)
}

// requireTemplateTestRunning checks a test that may have a workspace.
func requireTemplateTestRunning(t *testing.T, tt *coderv1alpha1.CoderTemplateTest, reason, messagePart string) {
	t.Helper()
	requireTemplateTestPhase(t, tt, coderv1alpha1.CoderTemplateTestPhaseRunning, reason, messagePart)
	require.NotNil(t, tt.Status.CreateAttemptTime, "Running means a create request may exist")
}

func requireTemplateTestPhase(t *testing.T, tt *coderv1alpha1.CoderTemplateTest, phase, reason, messagePart string) {
	t.Helper()
	require.Equal(t, phase, tt.Status.Phase, tt.Status.Message)
	require.Equal(t, reason, tt.Status.Reason, tt.Status.Message)
	require.Contains(t, tt.Status.Message, messagePart)
	require.True(t, meta.IsStatusConditionTrue(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionReconciling))
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))
}

func requireTemplateTestFailed(t *testing.T, tt *coderv1alpha1.CoderTemplateTest, reason string) {
	t.Helper()
	requireTemplateTestFailedWith(t, tt, reason, metav1.ConditionTrue, "NotCreated")
}

// requireTemplateTestFailedWith checks a failed test and its WorkspaceDeleted
// condition. The finalizer stays until the workspace is proven gone.
func requireTemplateTestFailedWith(t *testing.T, tt *coderv1alpha1.CoderTemplateTest, reason string, deletedStatus metav1.ConditionStatus, deletedReason string) {
	t.Helper()
	require.Equal(t, coderv1alpha1.CoderTemplateTestPhaseFailed, tt.Status.Phase, tt.Status.Message)
	require.Equal(t, reason, tt.Status.Reason, tt.Status.Message)
	require.NotNil(t, tt.Status.CompletionTime)
	require.True(t, meta.IsStatusConditionTrue(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionStalled))
	require.True(t, meta.IsStatusConditionFalse(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionReady))
	require.True(t, meta.IsStatusConditionFalse(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionReconciling))
	deleted := meta.FindStatusCondition(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted)
	require.NotNil(t, deleted)
	require.Equal(t, deletedStatus, deleted.Status)
	require.Equal(t, deletedReason, deleted.Reason)
	require.Equal(t, deletedStatus != metav1.ConditionTrue, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer),
		"the finalizer stays exactly while the workspace can exist")
}

func TestTemplateTestWaitsForControlPlaneAndOwner(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	requireStep := func(reason, messagePart string) *coderv1alpha1.CoderTemplateTest {
		t.Helper()
		tt := e.settle(t, key)
		requireTemplateTestWaiting(t, tt, reason, messagePart)
		require.Positive(t, e.lastStep.RequeueAfter)
		return tt
	}

	tt := e.reconcile(t, key, 1)
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))
	require.Empty(t, tt.Status.Phase, "the first reconcile only adds the finalizer")
	require.Zero(t, e.fake.requestCount(routeUser), "no Coder call before the finalizer is stored")

	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Status.OperatorAccessReady = false
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))
	tt = requireStep("OperatorAccessNotReady", "operator access")
	require.NotNil(t, tt.Status.StartTime)
	require.Equal(t, tt.Generation, tt.Status.ObservedGeneration)
	require.True(t, meta.IsStatusConditionFalse(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionReady))
	require.True(t, meta.IsStatusConditionFalse(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionStalled))
	cp.Status.OperatorAccessReady, cp.Status.URL = true, ""
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))
	requireStep("ControlPlaneNotReady", "status.url")
	require.NoError(t, k8sClient.Delete(e.ctx, cp))
	requireStep("ControlPlaneNotReady", "does not exist")

	// A recreated control plane starts without an owner.
	require.NoError(t, k8sClient.Delete(e.ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: "coder-operator-token"}}))
	createTestControlPlane(e.ctx, t, e.ns, "coder", e.fake.server.URL)
	requireStep("OwnerNotConfigured", "spec.templateTests.ownerUserID")
	e.setOwner(t, "coder", uuid.NewString())
	requireStep("OwnerNotEligible", "does not exist")
	e.setOwner(t, "coder", e.tester.String())
	e.fake.failNext(routeUser, fakeFault{Status: 503})
	requireTemplateTestWaiting(t, e.reconcile(t, key, 1), "CoderUnavailable", "get owner: Coder answered 503")
	requireTemplateTestRunning(t, e.settle(t, key), "WaitingForBuild", "")
	require.Equal(t, 1, e.fake.requestCount(routeCreateWorkspace))

	// Deleting a test that never sent a create request releases it at once.
	e.setOwner(t, "coder", "")
	other := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	requireTemplateTestWaiting(t, e.settle(t, other), "OwnerNotConfigured", "")
	require.NoError(t, k8sClient.Delete(e.ctx, &coderv1alpha1.CoderTemplateTest{ObjectMeta: metav1.ObjectMeta{Namespace: other.Namespace, Name: other.Name}}))
	require.Nil(t, e.reconcile(t, other, 1))
}

func TestTemplateTestWaitsForTemplateVersion(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "later.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	requireStep := func(reason, messagePart string) *coderv1alpha1.CoderTemplateTest {
		t.Helper()
		tt := e.settle(t, key)
		requireTemplateTestWaiting(t, tt, reason, messagePart)
		return tt
	}

	requireStep("TemplateNotFound", `organization "later"`)
	orgID := e.fake.addOrganization("later")
	requireStep("OwnerNotEligible", `not a member of organization "later"`)
	e.fake.addMember(orgID, e.tester)
	requireStep("TemplateNotFound", `template "later.docker"`)
	tplID := e.fake.addTemplate(orgID, "docker")
	requireStep("TemplateVersionNotFound", "does not exist")
	v1 := e.fake.addVersion(tplID, "v1", codersdk.ProvisionerJobRunning)
	tt := requireStep("TemplateVersionImporting", "still importing")
	require.Equal(t, v1.String(), tt.Status.TemplateVersionID, "the version is pinned while it imports")
	require.Empty(t, tt.Status.OwnerID, "the other inputs are pinned once the version is usable")
	e.fake.setVersionJob(v1, codersdk.ProvisionerJobSucceeded)
	tt = e.settle(t, key)
	requireTemplateTestRunning(t, tt, "WaitingForBuild", "")

	require.Equal(t, orgID.String(), tt.Status.OrganizationID)
	require.Equal(t, tplID.String(), tt.Status.TemplateID)
	require.Equal(t, v1.String(), tt.Status.TemplateVersionID)
	require.Equal(t, "v1", tt.Status.TemplateVersionName)
	require.Equal(t, e.tester.String(), tt.Status.OwnerID)
	require.Regexp(t, `^ktt-[0-9a-f]{28}$`, tt.Status.WorkspaceName)
	require.Equal(t, 1, e.fake.requestCount(routeCreateWorkspace))
}

func TestTemplateTestOwnerEligibility(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(e *templateTestEnv) uuid.UUID
		refusal string // Empty: the owner is eligible.
	}{
		{name: "extra site role", refusal: `site role "template-admin"`, setup: func(e *templateTestEnv) uuid.UUID {
			u := e.fake.addUser("t", codersdk.LoginTypePassword, codersdk.RoleTemplateAdmin)
			e.fake.addMember(e.orgID, u)
			return u
		}},
		{name: "admin role in the template organization", refusal: `role "organization-admin"`, setup: func(e *templateTestEnv) uuid.UUID {
			u := e.fake.addUser("t", codersdk.LoginTypePassword)
			e.fake.addMember(e.orgID, u, codersdk.RoleOrganizationAdmin)
			return u
		}},
		{name: "admin role in another organization", refusal: `role "organization-template-admin"`, setup: func(e *templateTestEnv) uuid.UUID {
			u := e.fake.addUser("t", codersdk.LoginTypePassword)
			e.fake.addMember(e.orgID, u)
			e.fake.addMember(e.fake.addOrganization("other"), u, codersdk.RoleOrganizationTemplateAdmin)
			return u
		}},
		{name: "admin role as a default member role of another organization", refusal: `default member role "organization-template-admin"`, setup: func(e *templateTestEnv) uuid.UUID {
			other := e.fake.addOrganization("other")
			e.fake.setDefaultMemberRoles(other, codersdk.RoleOrganizationWorkspaceAccess, codersdk.RoleOrganizationTemplateAdmin)
			e.fake.addMember(other, e.tester)
			return e.tester
		}},
		{name: "unknown organization role", refusal: `role "custom-builder"`, setup: func(e *templateTestEnv) uuid.UUID {
			u := e.fake.addUser("t", codersdk.LoginTypePassword)
			e.fake.addMember(e.orgID, u, "custom-builder")
			return u
		}},
		{name: "unknown user status", refusal: `status "disabled"`, setup: func(e *templateTestEnv) uuid.UUID {
			e.fake.updateUser(e.tester, func(u *codersdk.User) { u.Status = "disabled" })
			return e.tester
		}},
		{name: "suspended user", refusal: `status "suspended"`, setup: func(e *templateTestEnv) uuid.UUID {
			e.fake.updateUser(e.tester, func(u *codersdk.User) { u.Status = codersdk.UserStatusSuspended })
			return e.tester
		}},
		{name: "OIDC login", refusal: `login type "oidc"`, setup: func(e *templateTestEnv) uuid.UUID {
			u := e.fake.addUser("t", codersdk.LoginTypeOIDC)
			e.fake.addMember(e.orgID, u)
			return u
		}},
		{name: "not a member", refusal: `not a member of organization "default"`, setup: func(e *templateTestEnv) uuid.UUID {
			return e.fake.addUser("t", codersdk.LoginTypePassword)
		}},
		{name: "allowed roles of a dormant user", setup: func(e *templateTestEnv) uuid.UUID {
			u := e.fake.addUser("t", codersdk.LoginTypePassword, codersdk.RoleMember)
			e.fake.updateUser(u, func(u *codersdk.User) { u.Status = codersdk.UserStatusDormant })
			e.fake.addMember(e.orgID, u, codersdk.RoleOrganizationMember, codersdk.RoleOrganizationWorkspaceAccess)
			return u
		}},
		{name: "service account", setup: func(e *templateTestEnv) uuid.UUID {
			u := e.fake.addUser("t", codersdk.LoginTypeNone) //nolint:staticcheck // The controller still accepts it for older headless users.
			e.fake.updateUser(u, func(u *codersdk.User) { u.IsServiceAccount = true })
			e.fake.addMember(e.orgID, u)
			return u
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newTemplateTestEnv(t)
			e.setOwner(t, "coder", tc.setup(e).String())
			tt := e.settle(t, e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"}))
			if tc.refusal == "" {
				requireTemplateTestRunning(t, tt, "WaitingForBuild", "")
				return
			}
			requireTemplateTestWaiting(t, tt, "OwnerNotEligible", tc.refusal)
			require.Empty(t, tt.Status.OwnerID)
		})
	}
}

func TestTemplateTestPinsActiveVersion(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	slow := e.fake.addTemplate(e.orgID, "slow")
	first := e.fake.addVersion(slow, "first", codersdk.ProvisionerJobRunning)
	// Coder v2.37.2 matches organization and template names case-insensitively.
	key := e.createTest(t, "Default.Slow", coderv1alpha1.CoderTemplateTestVersion{Active: ptr.To(true)})
	tt := e.settle(t, key)
	requireTemplateTestWaiting(t, tt, "TemplateVersionImporting", "first")
	require.Equal(t, first.String(), tt.Status.TemplateVersionID, "the active version is pinned before its import ends")

	// Another version is promoted while the pinned one still imports.
	e.fake.promoteVersion(e.fake.addVersion(slow, "second", codersdk.ProvisionerJobSucceeded))
	tt = e.settle(t, key)
	requireTemplateTestWaiting(t, tt, "TemplateVersionImporting", "first")
	require.Equal(t, first.String(), tt.Status.TemplateVersionID, "a promotion after pinning does not change the version under test")

	e.fake.setVersionJob(first, codersdk.ProvisionerJobSucceeded)
	tt = e.settle(t, key)
	requireTemplateTestRunning(t, tt, "WaitingForBuild", "")
	require.Equal(t, "first", tt.Status.TemplateVersionName)
	ws, err := e.fake.client(t, 5*time.Second).Workspace(e.ctx, uuid.MustParse(tt.Status.WorkspaceID))
	require.NoError(t, err)
	require.Equal(t, first, ws.LatestBuild.TemplateVersionID, "the start build uses the pinned version")
}

func TestTemplateTestVersionFailures(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	otherTemplate := e.fake.addTemplate(e.orgID, "other")
	cases := map[string]coderv1alpha1.CoderTemplateTestVersion{
		"TemplateVersionMismatch":     {ID: e.fake.addVersion(otherTemplate, "v1", codersdk.ProvisionerJobSucceeded).String()},
		"TemplateVersionImportFailed": {Name: "broken"},
		"TemplateVersionArchived":     {Name: "old"},
	}
	e.fake.addVersion(e.tplID, "broken", codersdk.ProvisionerJobFailed)
	e.fake.archiveVersion(e.fake.addVersion(e.tplID, "old", codersdk.ProvisionerJobSucceeded))
	for reason, version := range cases {
		requireTemplateTestFailed(t, e.settle(t, e.createTest(t, "default.docker", version)), reason)
	}

	// Coder v2.37.2 refuses new workspaces for a deprecated template
	// (coderd/workspaces.go:958).
	legacy := e.fake.addTemplate(e.orgID, "legacy")
	e.fake.addVersion(legacy, "v1", codersdk.ProvisionerJobSucceeded)
	e.fake.deprecateTemplate(legacy)
	tt := e.settle(t, e.createTest(t, "default.legacy", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"}))
	requireTemplateTestFailed(t, tt, "TemplateDeprecated")
	require.NotContains(t, tt.Status.Message, "Use the new template", "the deprecation message is Coder detail")
}

// TestTemplateTestRejectsWrongCoderAnswers checks the rule for every Coder
// answer the reconciler trusts: it names what the controller asked for.
func TestTemplateTestRejectsWrongCoderAnswers(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	pass := fakeFault{Rewrite: func(a any) any { return a }}
	cases := []struct {
		name    string
		version coderv1alpha1.CoderTemplateTestVersion
		route   string
		faults  []fakeFault // The last one rewrites the answer.
		want    string
	}{
		{name: "user", route: routeUser, want: "user", faults: []fakeFault{{Rewrite: func(a any) any {
			u := a.(codersdk.User)
			u.ID = uuid.New()
			return u
		}}}},
		{name: "organization by ID", route: routeOrganization, want: "organization", faults: []fakeFault{{Rewrite: func(a any) any {
			o := a.(codersdk.Organization)
			o.ID = uuid.New()
			return o
		}}}},
		{name: "member user", route: routeOrgMember, want: "member user", faults: []fakeFault{{Rewrite: func(a any) any {
			m := a.(codersdk.OrganizationMemberWithUserData)
			m.UserID = uuid.New()
			return m
		}}}},
		{name: "member organization", route: routeOrgMember, want: "member organization", faults: []fakeFault{{Rewrite: func(a any) any {
			m := a.(codersdk.OrganizationMemberWithUserData)
			m.OrganizationID = uuid.New()
			return m
		}}}},
		{name: "organization by name", route: routeOrganization, want: "organization", faults: []fakeFault{pass, {Rewrite: func(a any) any {
			o := a.(codersdk.Organization)
			o.Name = "other"
			return o
		}}}},
		{name: "template", route: routeTemplateByName, want: "template", faults: []fakeFault{{Rewrite: func(a any) any {
			tpl := a.(codersdk.Template)
			tpl.Name = "other"
			return tpl
		}}}},
		{name: "template organization", route: routeTemplateByName, want: "template organization", faults: []fakeFault{{Rewrite: func(a any) any {
			tpl := a.(codersdk.Template)
			tpl.OrganizationID = uuid.New()
			return tpl
		}}}},
		{name: "version by name", route: routeVersionByName, want: "version", faults: []fakeFault{{Rewrite: func(a any) any {
			v := a.(codersdk.TemplateVersion)
			v.Name = "v2"
			return v
		}}}},
		{name: "version by ID", version: coderv1alpha1.CoderTemplateTestVersion{ID: e.v1.String()}, route: routeVersion, want: "version", faults: []fakeFault{{Rewrite: func(a any) any {
			v := a.(codersdk.TemplateVersion)
			v.ID = uuid.New()
			return v
		}}}},
		{name: "active version", version: coderv1alpha1.CoderTemplateTestVersion{Active: ptr.To(true)}, route: routeVersion, want: "version", faults: []fakeFault{{Rewrite: func(a any) any {
			v := a.(codersdk.TemplateVersion)
			v.ID = uuid.New()
			return v
		}}}},
	}
	for _, tc := range cases {
		version := tc.version
		if version == (coderv1alpha1.CoderTemplateTestVersion{}) {
			version.Name = "v1"
		}
		key := e.createTest(t, "default.docker", version)
		for _, fault := range tc.faults {
			e.fake.failNext(tc.route, fault)
		}
		// Finalizer, initialization, then the lookups that get the wrong answer.
		requireTemplateTestWaiting(t, e.reconcile(t, key, 3), "CoderAnswerMismatch", "assertion failed: Coder answered "+tc.want+" ")
		require.Zero(t, e.fake.requestCount(routeCreateWorkspace), tc.name)
	}

	// A pinned version is read by ID, and the answer must name that ID.
	slow := e.fake.addTemplate(e.orgID, "slow")
	e.fake.addVersion(slow, "first", codersdk.ProvisionerJobRunning)
	key := e.createTest(t, "default.slow", coderv1alpha1.CoderTemplateTestVersion{Active: ptr.To(true)})
	requireTemplateTestWaiting(t, e.settle(t, key), "TemplateVersionImporting", "")
	e.fake.failNext(routeVersion, fakeFault{Rewrite: func(a any) any {
		v := a.(codersdk.TemplateVersion)
		v.ID = uuid.New()
		return v
	}})
	requireTemplateTestWaiting(t, e.reconcile(t, key, 1), "CoderAnswerMismatch", "assertion failed: Coder answered version ")
}

// TestTemplateTestWrongAnswerEndsAtDeadline checks that a Coder that keeps
// answering wrong ends the test at its deadline, not after the work queue's
// backoff.
func TestTemplateTestWrongAnswerEndsAtDeadline(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	for range 20 {
		e.fake.failNext(routeUser, fakeFault{Rewrite: func(a any) any {
			u := a.(codersdk.User)
			u.ID = uuid.New()
			return u
		}})
	}
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	requireTemplateTestWaiting(t, e.settle(t, key), "CoderAnswerMismatch", "")
	require.Equal(t, 15*time.Second, e.lastStep.RequeueAfter, "a wrong answer requeues like any wait, without an error")

	e.clock.SetTime(e.clock.Now().Add(900 * time.Second))
	tt := e.settle(t, key)
	requireTemplateTestFailed(t, tt, "DeadlineExceeded")
	require.Contains(t, tt.Status.Message, "Last wait: CoderAnswerMismatch")
}

func TestTemplateTestDeadlineWhilePending(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	e.setOwner(t, "coder", "")
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	requireTemplateTestWaiting(t, e.settle(t, key), "OwnerNotConfigured", "")

	e.clock.SetTime(e.clock.Now().Add(899 * time.Second))
	requireTemplateTestWaiting(t, e.settle(t, key), "OwnerNotConfigured", "")
	require.Equal(t, time.Second, e.lastStep.RequeueAfter, "the requeue never passes the deadline")

	e.clock.SetTime(e.clock.Now().Add(time.Second))
	tt := e.settle(t, key)
	requireTemplateTestFailed(t, tt, "DeadlineExceeded")
	require.Contains(t, tt.Status.Message, "Last wait: OwnerNotConfigured")
	require.Equal(t, tt.Status.CompletionTime, e.settle(t, key).Status.CompletionTime, "a final test is never evaluated again")
}

// lateClock answers its times in order and then repeats the last one, like
// a reconcile whose Coder calls take a while.
type lateClock struct {
	times []time.Time
	calls int
}

func (c *lateClock) Now() time.Time {
	c.calls++
	return c.times[min(c.calls, len(c.times))-1]
}

func (c *lateClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

func TestTemplateTestDeadlineAfterSlowLookup(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	tt := e.reconcile(t, key, 2) // Finalizer and initialization only.
	require.NotNil(t, tt.Status.StartTime)

	deadline := tt.Status.StartTime.Add(900 * time.Second)
	r := &controller.CoderTemplateTestReconciler{Client: k8sClient, Scheme: scheme, Clock: &lateClock{times: []time.Time{deadline.Add(-time.Second), deadline}}}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	tt = e.settle(t, key)
	requireTemplateTestFailed(t, tt, "DeadlineExceeded")
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace), "no create request after the deadline")
}

func TestTemplateTestNameConflict(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})

	// Someone else takes the test's workspace name before the create step.
	sdk := e.fake.client(t, 5*time.Second)
	foreign, err := sdk.CreateUserWorkspace(e.ctx, e.tester.String(), codersdk.CreateWorkspaceRequest{TemplateVersionID: e.v1, Name: e.workspaceName(t, key)})
	require.NoError(t, err)

	requireTemplateTestFailed(t, e.settle(t, key), "WorkspaceNameConflict")
	require.Equal(t, 1, e.fake.requestCount(routeCreateWorkspace), "only the foreign create request")
	require.Zero(t, e.fake.requestCount(routeCreateBuild)+e.fake.requestCount(routeCancelBuild), "the controller never touches the foreign workspace")
	got, err := sdk.Workspace(e.ctx, foreign.ID)
	require.NoError(t, err)
	require.Equal(t, foreign.LatestBuild.ID, got.LatestBuild.ID)
}
