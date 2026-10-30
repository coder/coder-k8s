package storage

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

func TestLineTail(t *testing.T) {
	push := func(tail *lineTail, lines ...string) *lineTail {
		for _, line := range lines {
			tail.push(line)
		}
		return tail
	}
	cases := []struct {
		name     string
		tail     *lineTail
		want     string
		wantsCut bool
	}{
		{"keeps the last lines", push(newLineTail(2, 100), "a\n", "b\n", "c\n"), "b\nc\n", false},
		{"fewer lines than asked", push(newLineTail(5, 100), "a\n", "b\n"), "a\nb\n", false},
		{"zero lines", push(newLineTail(0, 100), "a\n"), "", false},
		{"byte cap drops the oldest lines", push(newLineTail(10, 5), "aa\n", "bb\n", "cc\n"), "cc\n", true},
		{"lines that fit the byte cap exactly", push(newLineTail(10, 6), "aa\n", "bb\n"), "aa\nbb\n", false},
		{"a line over the byte cap keeps its start", push(newLineTail(10, 4), "a\n", "üüü\n"), "üü", true},
	}
	for _, tc := range cases {
		if got := tc.tail.String(); got != tc.want || tc.tail.cut != tc.wantsCut {
			t.Errorf("%s: got %q cut=%v, want %q cut=%v", tc.name, got, tc.tail.cut, tc.want, tc.wantsCut)
		}
	}

	// A huge tailLines does not lift the byte cap.
	huge := newLineTail(1<<62, 1000)
	for i := 0; i < 100_000; i++ {
		huge.push("0123456789\n")
	}
	if got := huge.String(); got != strings.Repeat("0123456789\n", 90) || !huge.cut {
		t.Fatalf("huge tailLines: len=%d cut=%v, want the newest 90 lines and cut", len(got), huge.cut)
	}
}

func TestWorkspaceLogTailLines(t *testing.T) {
	f := newLogFakeCoder(t)
	s := newTestLogStorage(t, f, nil)
	entries := testLogEntries(5)
	f.setLogs(entries)

	cases := []struct {
		name  string
		opts  *aggregationv1alpha1.CoderWorkspaceLogOptions
		want  string
		calls int32
	}{
		{"last 2 lines", &aggregationv1alpha1.CoderWorkspaceLogOptions{TailLines: int64Ptr(2)}, renderedLog(entries[3:]), 2},
		{"more lines than the log", &aggregationv1alpha1.CoderWorkspaceLogOptions{TailLines: int64Ptr(10)}, renderedLog(entries), 2},
		{"zero lines", &aggregationv1alpha1.CoderWorkspaceLogOptions{TailLines: int64Ptr(0)}, "", 2},
		// As for Pod logs, tailLines applies first and limitBytes to its result.
		{"tail, then limitBytes", &aggregationv1alpha1.CoderWorkspaceLogOptions{TailLines: int64Ptr(2), LimitBytes: int64Ptr(30)}, renderedLog(entries[3:])[:30], 2},
	}
	for _, tc := range cases {
		before := f.requests.Load()
		got, warnings, err := readLog(t, s, logTestName, tc.opts)
		if err != nil || got != tc.want || len(warnings) != 0 {
			t.Errorf("%s: err=%v warnings=%v got=%q want=%q", tc.name, err, warnings, got, tc.want)
		}
		// The documented cost of a log request does not change with tailLines.
		if calls := f.requests.Load() - before; calls != tc.calls {
			t.Errorf("%s: %d Coder requests, want %d", tc.name, calls, tc.calls)
		}
	}

	_, err := s.Get(logRequestContext(t.Context(), "alice", &recordedWarnings{}), logTestName,
		&aggregationv1alpha1.CoderWorkspaceLogOptions{TailLines: int64Ptr(-1)})
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "tailLines") {
		t.Fatalf("tailLines=-1: err=%v, want Invalid for tailLines", err)
	}
	if s.slots.inUse() != 0 {
		t.Fatalf("slots in use: %d", s.slots.inUse())
	}
}

func TestWorkspaceLogTailServerCapsWarn(t *testing.T) {
	f := newLogFakeCoder(t)
	entries := testLogEntries(50)
	f.setLogs(entries)
	lineLen := int64(len(entries[49].Text()) + 1)

	byteCapped := newTestLogStorage(t, f, func(l *workspaceLogLimits) { l.maxBytes = 2*lineLen + 1 })
	got, warnings, err := readLog(t, byteCapped, logTestName, &aggregationv1alpha1.CoderWorkspaceLogOptions{TailLines: int64Ptr(5)})
	if err != nil || got != renderedLog(entries[48:]) {
		t.Fatalf("tail over the byte cap must keep the newest lines that fit: err=%v got=%q", err, got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "tail truncated at the server limit") {
		t.Fatalf("tail over the byte cap: warnings=%v", warnings)
	}

	// Coder returns the log from the start, so past the read cap the tail is of the part read.
	scanCapped := newTestLogStorage(t, f, func(l *workspaceLogLimits) { l.maxScanBytes = 1000 })
	head, _, err := readLog(t, scanCapped, logTestName, nil)
	if err != nil {
		t.Fatal(err)
	}
	read := strings.SplitAfter(head, "\n")
	read = read[:len(read)-1] // SplitAfter leaves an empty string after the last newline
	got, warnings, err = readLog(t, scanCapped, logTestName, &aggregationv1alpha1.CoderWorkspaceLogOptions{TailLines: int64Ptr(2)})
	if err != nil || len(read) < 3 || got != strings.Join(read[len(read)-2:], "") {
		t.Fatalf("tail past the read cap: err=%v got=%q read=%d lines", err, got, len(read))
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], "read limit of 1000 bytes") || !strings.Contains(warnings[1], "tailLines counts from the end of the part") {
		t.Fatalf("tail past the read cap: warnings=%v", warnings)
	}
	for _, w := range warnings {
		if strings.Contains(w, "line ") {
			t.Fatalf("warning leaks log content: %q", w)
		}
	}
}

func TestWorkspaceLogTailCutsOnRuneBoundary(t *testing.T) {
	f := newLogFakeCoder(t)
	entries := testLogEntries(3)
	for i := range entries {
		entries[i].Output = "héllo wörld ✓ " + entries[i].Output
	}
	f.setLogs(entries)
	tail := renderedLog(entries[1:])
	s := newTestLogStorage(t, f, nil)
	for limit := int64(60); limit < 100; limit++ {
		got, _, err := readLog(t, s, logTestName, &aggregationv1alpha1.CoderWorkspaceLogOptions{TailLines: int64Ptr(2), LimitBytes: int64Ptr(limit)})
		if err != nil || !utf8.ValidString(got) || !strings.HasPrefix(tail, got) || int64(len(got)) > limit || int64(len(got)) < limit-3 {
			t.Fatalf("tailLines=2 limitBytes=%d: err=%v valid=%v len=%d got=%q", limit, err, utf8.ValidString(got), len(got), got)
		}
	}
}

// TestWorkspaceLogFollowWithTailLines: with follow, the last lines of the existing log come first,
// and the live part continues after the last entry read, not the last entry shown.
func TestWorkspaceLogFollowWithTailLines(t *testing.T) {
	f := newLogFakeCoder(t)
	entries := testLogEntries(4)
	f.setLogs(entries[:3])
	s := newTestLogStorage(t, f, nil)

	for tailLines, want := range map[int64]string{1: renderedLog(entries[2:]), 0: renderedLog(entries[3:])} {
		ff := serveFollow(t, f, entries[3:], nil)
		before := f.requests.Load()
		body := openFollow(t, s, &aggregationv1alpha1.CoderWorkspaceLogOptions{Follow: true, TailLines: int64Ptr(tailLines)})
		got, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil || string(got) != want {
			t.Fatalf("follow tailLines=%d: err=%v got=%q want=%q", tailLines, err, got, want)
		}
		if after := ff.after.Load(); after != entries[2].ID {
			t.Fatalf("follow tailLines=%d continued after %d, want the last entry read %d", tailLines, after, entries[2].ID)
		}
		if calls := f.requests.Load() - before; calls != 3 {
			t.Fatalf("follow tailLines=%d made %d Coder requests, want 3", tailLines, calls)
		}
	}
}

// TestWorkspaceLogFollowWithCutTail: a tail that limitBytes or the server cap cut ends the
// response without opening a Coder stream, as a cut snapshot does.
func TestWorkspaceLogFollowWithCutTail(t *testing.T) {
	f := newLogFakeCoder(t)
	entries := testLogEntries(5)
	f.setLogs(entries)
	var dials atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dials.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	f.followHandler.Store(&h)

	s := newTestLogStorage(t, f, nil)
	got, _, err := readLog(t, s, logTestName, &aggregationv1alpha1.CoderWorkspaceLogOptions{Follow: true, TailLines: int64Ptr(2), LimitBytes: int64Ptr(10)})
	if err != nil || got != renderedLog(entries[3:])[:10] || dials.Load() != 0 {
		t.Fatalf("tail cut by limitBytes with follow: got=%q err=%v dials=%d", got, err, dials.Load())
	}

	lineLen := int64(len(entries[0].Text()) + 1)
	capped := newTestLogStorage(t, f, func(l *workspaceLogLimits) { l.maxBytes = 2*lineLen + 1 })
	got, warnings, err := readLog(t, capped, logTestName, &aggregationv1alpha1.CoderWorkspaceLogOptions{Follow: true, TailLines: int64Ptr(3)})
	if err != nil || got != renderedLog(entries[3:]) || len(warnings) != 1 || dials.Load() != 0 {
		t.Fatalf("tail cut by the server cap with follow: got=%q warnings=%v err=%v dials=%d", got, warnings, err, dials.Load())
	}
}
