package coder

import (
	"strings"
	"testing"
)

func TestParseTemplateName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantOrg   string
		wantTmpl  string
		wantError bool
	}{
		{name: "valid", input: "acme.starter", wantOrg: "acme", wantTmpl: "starter"},
		{name: "empty input", input: "", wantError: true},
		{name: "missing separator", input: "acme", wantError: true},
		{name: "too many separators", input: "acme.team.starter", wantError: true},
		{name: "empty organization", input: ".starter", wantError: true},
		{name: "empty template", input: "acme.", wantError: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			org, template, err := ParseTemplateName(testCase.input)
			if testCase.wantError {
				if err == nil {
					t.Fatalf("expected error for input %q", testCase.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %q: %v", testCase.input, err)
			}
			if org != testCase.wantOrg {
				t.Fatalf("expected organization %q, got %q", testCase.wantOrg, org)
			}
			if template != testCase.wantTmpl {
				t.Fatalf("expected template %q, got %q", testCase.wantTmpl, template)
			}
		})
	}
}

func TestParseWorkspaceName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		wantOrg       string
		wantUser      string
		wantWorkspace string
		wantError     bool
	}{
		{name: "valid", input: "acme.alice.dev", wantOrg: "acme", wantUser: "alice", wantWorkspace: "dev"},
		{name: "empty input", input: "", wantError: true},
		{name: "missing separator", input: "acme", wantError: true},
		{name: "too few separators", input: "acme.alice", wantError: true},
		{name: "too many separators", input: "acme.alice.team.dev", wantError: true},
		{name: "empty organization", input: ".alice.dev", wantError: true},
		{name: "empty user", input: "acme..dev", wantError: true},
		{name: "empty workspace", input: "acme.alice.", wantError: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			org, user, workspace, err := ParseWorkspaceName(testCase.input)
			if testCase.wantError {
				if err == nil {
					t.Fatalf("expected error for input %q", testCase.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %q: %v", testCase.input, err)
			}
			if org != testCase.wantOrg {
				t.Fatalf("expected organization %q, got %q", testCase.wantOrg, org)
			}
			if user != testCase.wantUser {
				t.Fatalf("expected user %q, got %q", testCase.wantUser, user)
			}
			if workspace != testCase.wantWorkspace {
				t.Fatalf("expected workspace %q, got %q", testCase.wantWorkspace, workspace)
			}
		})
	}
}

func TestBuildTemplateName(t *testing.T) {
	t.Parallel()

	if got, want := BuildTemplateName("acme", "starter"), "acme.starter"; got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestBuildWorkspaceName(t *testing.T) {
	t.Parallel()

	if got, want := BuildWorkspaceName("acme", "alice", "dev"), "acme.alice.dev"; got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestBuildNamePanicsForInvalidSegments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fn   func()
	}{
		{
			name: "empty org in template",
			fn: func() {
				_ = BuildTemplateName("", "starter")
			},
		},
		{
			name: "dot in template segment",
			fn: func() {
				_ = BuildTemplateName("acme", "starter.v2")
			},
		},
		{
			name: "empty workspace segment",
			fn: func() {
				_ = BuildWorkspaceName("acme", "alice", "")
			},
		},
		{
			name: "dot in user segment",
			fn: func() {
				_ = BuildWorkspaceName("acme", "alice.dev", "workspace")
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			expectAssertionPanic(t, testCase.fn)
		})
	}
}

func expectAssertionPanic(t *testing.T, fn func()) {
	t.Helper()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected panic, got nil")
		}

		message, ok := recovered.(string)
		if !ok {
			t.Fatalf("expected panic string, got %T (%v)", recovered, recovered)
		}
		if !strings.HasPrefix(message, "assertion failed:") {
			t.Fatalf("expected assertion panic, got %q", message)
		}
	}()

	fn()
}
