package allapp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"
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

type recordingManager struct {
	manager.Manager
	added []manager.Runnable
}

func (m *recordingManager) Add(r manager.Runnable) error {
	m.added = append(m.added, r)
	return nil
}

// TestAddRunnablesRegistersOnlyAggregatedAPIServer guards the security decision that --app=all never
// starts the MCP HTTP server.
func TestAddRunnablesRegistersOnlyAggregatedAPIServer(t *testing.T) {
	mgr := &recordingManager{}
	if err := addRunnables(mgr, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(mgr.added) != 1 {
		t.Fatalf("expected exactly one runnable (aggregated-apiserver), got %d", len(mgr.added))
	}
	runnable, ok := mgr.added[0].(nonLeaderRunnable)
	if !ok || runnable.run == nil {
		t.Fatalf("expected a nonLeaderRunnable with a run function, got %T", mgr.added[0])
	}
}

func TestAddRunnablesRejectsInvalidArguments(t *testing.T) {
	if err := addRunnables(nil, time.Second); err == nil || !strings.Contains(err.Error(), "manager must not be nil") {
		t.Fatalf("expected nil manager assertion, got %v", err)
	}
	if err := addRunnables(&recordingManager{}, 0); err == nil || !strings.Contains(err.Error(), "request timeout must be positive") {
		t.Fatalf("expected request timeout assertion, got %v", err)
	}
}
