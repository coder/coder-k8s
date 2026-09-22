package convert

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
)

// versionFixtureWorkspace returns a workspace whose UpdatedAt never advances, modelling the Coder
// 2.37.2 behavior observed in #108/#109: builds, rename, TTL and autostart changes left updated_at
// untouched.
func versionFixtureWorkspace() codersdk.Workspace {
	fixed := time.Date(2026, time.September, 22, 13, 25, 32, 675817000, time.UTC)
	ttl := int64(3600000)
	autostart := "CRON_TZ=UTC 0 9 * * 1-5"

	return codersdk.Workspace{
		ID:                uuid.MustParse("0a812ea0-ea77-498c-84fe-eb53676f8fe5"),
		CreatedAt:         fixed,
		UpdatedAt:         fixed,
		OwnerName:         "alice",
		OrganizationName:  "acme",
		TemplateName:      "starter-template",
		Name:              "dev-workspace",
		TTLMillis:         &ttl,
		AutostartSchedule: &autostart,
		LastUsedAt:        fixed,
		LatestBuild: codersdk.WorkspaceBuild{
			ID:                uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			BuildNumber:       1,
			TemplateVersionID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
			Transition:        codersdk.WorkspaceTransitionStart,
			Status:            codersdk.WorkspaceStatusRunning,
			CreatedAt:         fixed,
			UpdatedAt:         fixed,
			Deadline:          codersdk.NewNullTime(fixed.Add(8*time.Hour), true),
		},
	}
}

func TestWorkspaceToK8sResourceVersionIsOpaqueSHA256(t *testing.T) {
	t.Parallel()

	converted := WorkspaceToK8s("control-plane", versionFixtureWorkspace())
	if len(converted.ResourceVersion) != hex.EncodedLen(32) {
		t.Fatalf("expected a full hex SHA-256 resourceVersion, got %q", converted.ResourceVersion)
	}
	if _, err := hex.DecodeString(converted.ResourceVersion); err != nil {
		t.Fatalf("expected hex resourceVersion, got %q: %v", converted.ResourceVersion, err)
	}
	if converted.UID != "0a812ea0-ea77-498c-84fe-eb53676f8fe5" {
		t.Fatalf("expected UID to stay the workspace ID, got %q", converted.UID)
	}
	if converted.Name != "acme.alice.dev-workspace" {
		t.Fatalf("expected canonical name, got %q", converted.Name)
	}
}

func TestWorkspaceToK8sResourceVersionStableForIdenticalProjection(t *testing.T) {
	t.Parallel()

	base := versionFixtureWorkspace()
	first := WorkspaceToK8s("control-plane", base).ResourceVersion
	second := WorkspaceToK8s("control-plane", base).ResourceVersion
	if first != second {
		t.Fatalf("identical input must yield identical resourceVersion: %q vs %q", first, second)
	}

	// Backend-only changes that this API does not expose must not change the token.
	backendOnly := base
	backendOnly.UpdatedAt = base.UpdatedAt.Add(time.Hour)
	backendOnly.LatestBuild.UpdatedAt = base.LatestBuild.UpdatedAt.Add(time.Hour)
	backendOnly.LatestBuild.CreatedAt = base.LatestBuild.CreatedAt.Add(time.Hour)
	backendOnly.LatestBuild.BuildNumber = 7
	if got := WorkspaceToK8s("control-plane", backendOnly).ResourceVersion; got != first {
		t.Fatalf("backend-only timestamp/build-number change must keep the token, got %q want %q", got, first)
	}

	// Returning to an identical projection returns the same token (not a history cursor).
	ttl := int64(7200000)
	changed := base
	changed.TTLMillis = &ttl
	reverted := changed
	reverted.TTLMillis = base.TTLMillis
	if got := WorkspaceToK8s("control-plane", reverted).ResourceVersion; got != first {
		t.Fatalf("reverted projection must return the original token, got %q want %q", got, first)
	}
}

func TestWorkspaceToK8sResourceVersionTracksExposedFields(t *testing.T) {
	t.Parallel()

	base := versionFixtureWorkspace()
	baseVersion := WorkspaceToK8s("control-plane", base).ResourceVersion

	newBuildID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	newTemplateVersionID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	newTTL := int64(7200000)
	newAutostart := "CRON_TZ=UTC 30 9 * * 1-5"

	cases := map[string]func(*codersdk.Workspace){
		"rename":           func(w *codersdk.Workspace) { w.Name = "dev-renamed" },
		"template name":    func(w *codersdk.Workspace) { w.TemplateName = "other-template" },
		"template version": func(w *codersdk.Workspace) { w.LatestBuild.TemplateVersionID = newTemplateVersionID },
		"new build (stop)": func(w *codersdk.Workspace) {
			w.LatestBuild.ID = newBuildID
			w.LatestBuild.Transition = codersdk.WorkspaceTransitionStop
			w.LatestBuild.Status = codersdk.WorkspaceStatusStopped
		},
		"build status only":     func(w *codersdk.Workspace) { w.LatestBuild.Status = codersdk.WorkspaceStatusStopping },
		"build id only":         func(w *codersdk.Workspace) { w.LatestBuild.ID = newBuildID },
		"ttl":                   func(w *codersdk.Workspace) { w.TTLMillis = &newTTL },
		"autostart":             func(w *codersdk.Workspace) { w.AutostartSchedule = &newAutostart },
		"lastUsedAt (activity)": func(w *codersdk.Workspace) { w.LastUsedAt = w.LastUsedAt.Add(time.Minute) },
		"autoShutdown (deadline)": func(w *codersdk.Workspace) {
			w.LatestBuild.Deadline = codersdk.NewNullTime(w.LatestBuild.Deadline.Time.Add(time.Hour), true)
		},
		"creation timestamp": func(w *codersdk.Workspace) { w.CreatedAt = w.CreatedAt.Add(-time.Hour) },
		"owner":              func(w *codersdk.Workspace) { w.OwnerName = "bob" },
		"organization":       func(w *codersdk.Workspace) { w.OrganizationName = "other" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			w := base
			mutate(&w)
			converted := WorkspaceToK8s("control-plane", w)
			if converted.ResourceVersion == baseVersion {
				t.Fatalf("exposed field changed but resourceVersion stayed %q", baseVersion)
			}
			if converted.UID != "0a812ea0-ea77-498c-84fe-eb53676f8fe5" {
				t.Fatalf("UID must stay the workspace ID, got %q", converted.UID)
			}
		})
	}
}

func TestWorkspaceToK8sResourceVersionIdentityAndNamespace(t *testing.T) {
	t.Parallel()

	base := versionFixtureWorkspace()
	converted := WorkspaceToK8s("control-plane", base)

	recreated := base
	recreated.ID = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	recreatedConverted := WorkspaceToK8s("control-plane", recreated)
	if recreatedConverted.UID == converted.UID {
		t.Fatal("recreated workspace (new ID, same name) must expose a new UID")
	}
	if recreatedConverted.ResourceVersion == converted.ResourceVersion {
		t.Fatal("recreated workspace must expose a different resourceVersion")
	}

	if other := WorkspaceToK8s("other-namespace", base); other.ResourceVersion == converted.ResourceVersion {
		t.Fatal("namespace is part of the projection and must change the resourceVersion")
	}
}
