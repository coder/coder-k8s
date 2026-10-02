package storage

import (
	"io"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// TestWorkspaceLogSlots: the server and per-user caps answer 429 before any Coder call, and every
// slot is released.
func TestWorkspaceLogSlots(t *testing.T) {
	f := newLogFakeCoder(t)
	f.setLogs(testLogEntries(1))
	s := newTestLogStorage(t, f, func(l *workspaceLogLimits) { l.maxStreams, l.maxStreamsPerUser = 2, 1 })

	open := func(userName string) (io.ReadCloser, error) {
		body, _, err := openLog(logRequestContext(t.Context(), userName, &recordedWarnings{}), s, logTestName, nil)
		return body, err
	}
	aliceStream, err := open("alice")
	if err != nil {
		t.Fatal(err)
	}
	lookups := f.lookups.Load()
	if _, err := open("alice"); !apierrors.IsTooManyRequests(err) || !strings.Contains(err.Error(), "for this user") {
		t.Fatalf("second stream for alice: err=%v, want per-user 429", err)
	}
	bobStream, err := open("bob")
	if err != nil {
		t.Fatalf("bob must not be limited by alice's slot: %v", err)
	}
	lookups++
	if _, err := open("carol"); !apierrors.IsTooManyRequests(err) || !strings.Contains(err.Error(), "on this server") {
		t.Fatalf("third stream: err=%v, want server 429", err)
	}
	if got := f.lookups.Load(); got != lookups {
		t.Fatalf("429 answers reached Coder: lookups %d, want %d", got, lookups)
	}
	if secs, ok := apierrors.SuggestsClientDelay(func() error { _, err := open("carol"); return err }()); !ok || secs != logSlotRetryAfterSeconds {
		t.Fatalf("Retry-After: %d %v", secs, ok)
	}

	_ = aliceStream.Close()
	_ = aliceStream.Close() // a second Close must not release twice
	_ = bobStream.Close()
	if s.slots.inUse() != 0 {
		t.Fatalf("slots in use after Close: %d", s.slots.inUse())
	}
	for i := 0; i < 3; i++ {
		body, err := open("alice")
		if err != nil {
			t.Fatalf("sequential stream %d: %v", i, err)
		}
		_ = body.Close()
	}
}
