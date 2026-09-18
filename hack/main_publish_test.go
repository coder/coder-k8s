package hack_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"text/template"

	"sigs.k8s.io/yaml"
)

func TestChangelogChannels(t *testing.T) {
	data, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Changelog struct {
			Disable string `json:"disable"`
			Use     string `json:"use"`
		} `json:"changelog"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config.Changelog.Use != "github" {
		t.Fatal("tagged releases must retain GitHub changelogs")
	}
	tmpl, err := template.New("disable").Parse(config.Changelog.Disable)
	if err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"main", "", "release"} {
		t.Run("channel="+channel, func(t *testing.T) {
			var result strings.Builder
			data := struct{ Env map[string]string }{Env: map[string]string{"GORELEASER_CHANNEL": channel}}
			if err := tmpl.Execute(&result, data); err != nil {
				t.Fatal(err)
			}
			disabled := result.String() == "true"
			if disabled != (channel == "main") {
				t.Fatalf("changelog disabled = %q for channel %q", result.String(), channel)
			}
		})
	}
}

func TestMainPublishTag(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				ID   string            `json:"id"`
				Run  string            `json:"run"`
				Uses string            `json:"uses"`
				Env  map[string]string `json:"env"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	var prepare, tag string
	prepareIndex, releaseIndex := -1, -1
	for i, step := range workflow.Jobs["publish-main"].Steps {
		if step.ID == "prepare-main-tag" {
			prepare, prepareIndex = step.Run, i
		}
		if strings.HasPrefix(step.Uses, "goreleaser/goreleaser-action@") {
			tag, releaseIndex = step.Env["GORELEASER_CURRENT_TAG"], i
		}
	}
	if tag == "" || releaseIndex < 0 {
		t.Fatal("main publisher must select a GoReleaser tag")
	}
	if prepareIndex >= releaseIndex {
		t.Fatal("main tag must be prepared before GoReleaser")
	}

	for _, stale := range []bool{false, true} {
		name := "absent"
		if stale {
			name = "stale"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			run := func(program string, args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), program, args...)
				cmd.Dir = dir
				cmd.Env = []string{
					"PATH=" + os.Getenv("PATH"), "HOME=" + dir,
					"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
				}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s %v: %v\n%s", program, args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			run("git", "init", "--initial-branch=main")
			run("git", "config", "user.name", "Test")
			run("git", "config", "user.email", "test@example.com")
			run("git", "commit", "--allow-empty", "-m", "release")
			releaseCommit := run("git", "rev-parse", "HEAD")
			run("git", "tag", "v1.0.0")
			if stale {
				run("git", "tag", tag)
			}
			run("git", "commit", "--allow-empty", "-m", "next main")

			// Execute only the local-tag step, never the publisher or login steps.
			run("bash", "-euo", "pipefail", "-c", prepare)
			// This is GoReleaser's non-snapshot tag validation predicate.
			if got := run("git", "describe", "--exact-match", "--tags", "--match", tag); got != tag {
				t.Fatalf("main tag = %q, want %q", got, tag)
			}
			if got := run("git", "rev-parse", "v1.0.0"); got != releaseCommit {
				t.Fatalf("release tag moved from %s to %s", releaseCommit, got)
			}
			if got := run("git", "status", "--porcelain"); got != "" {
				t.Fatalf("tag preparation dirtied the worktree: %s", got)
			}
		})
	}
}
