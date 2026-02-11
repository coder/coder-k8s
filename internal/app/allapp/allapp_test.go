package allapp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunRejectsNilContext(t *testing.T) {
	t.Helper()

	var nilCtx context.Context
	err := Run(nilCtx, 30*time.Second)
	if err == nil {
		t.Fatal("expected an error when context is nil")
	}
	if !strings.Contains(err.Error(), "context must not be nil") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNonLeaderRunnableNeedLeaderElection(t *testing.T) {
	t.Helper()

	runnable := nonLeaderRunnable{}
	if runnable.NeedLeaderElection() {
		t.Fatal("expected non-leader runnable to disable leader election")
	}
}

func TestNonLeaderRunnableStartCallsRun(t *testing.T) {
	t.Helper()

	expectedErr := errors.New("sentinel runnable error")
	called := false
	runnable := nonLeaderRunnable{
		run: func(ctx context.Context) error {
			called = true
			if ctx == nil {
				t.Fatal("expected non-nil context")
			}
			return expectedErr
		},
	}

	err := runnable.Start(context.Background())
	if !called {
		t.Fatal("expected runnable callback to be called")
	}
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected sentinel error %v, got %v", expectedErr, err)
	}
}

func TestNonLeaderRunnableStartRequiresRunFunction(t *testing.T) {
	t.Helper()

	err := nonLeaderRunnable{}.Start(context.Background())
	if err == nil {
		t.Fatal("expected an error when runnable callback is nil")
	}
	if !strings.Contains(err.Error(), "runnable function must not be nil") {
		t.Fatalf("unexpected error: %v", err)
	}
}
