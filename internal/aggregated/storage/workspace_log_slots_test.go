package storage

import (
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func TestLogSlotsCaps(t *testing.T) {
	limits := defaultWorkspaceLogLimits()
	limits.maxStreams, limits.maxStreamsPerUser = 3, 2
	slots := &logSlots{limits: limits, perUser: map[string]int{}}

	a1, err := slots.acquire("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slots.acquire("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := slots.acquire("alice"); !apierrors.IsTooManyRequests(err) {
		t.Fatalf("third alice slot: err=%v, want 429", err)
	}
	if _, err := slots.acquire("bob"); err != nil {
		t.Fatalf("bob: %v", err)
	}
	_, err = slots.acquire("carol")
	if !apierrors.IsTooManyRequests(err) {
		t.Fatalf("server cap: err=%v, want 429", err)
	}
	if secs, ok := apierrors.SuggestsClientDelay(err); !ok || secs != logSlotRetryAfterSeconds {
		t.Fatalf("Retry-After: %d %v", secs, ok)
	}

	a1()
	a1() // releasing twice frees one slot only
	if got := slots.inUse(); got != 2 {
		t.Fatalf("slots in use: %d, want 2", got)
	}
	if _, err := slots.acquire("carol"); err != nil {
		t.Fatalf("carol after a release: %v", err)
	}
	if _, err := slots.acquire("dave"); !apierrors.IsTooManyRequests(err) {
		t.Fatalf("a double release must not free a second slot: err=%v", err)
	}
}

func TestWorkspaceLogLimitsValidate(t *testing.T) {
	if err := defaultWorkspaceLogLimits().validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*workspaceLogLimits){
		"per-user above server": func(l *workspaceLogLimits) { l.maxStreamsPerUser = l.maxStreams + 1 },
		"zero read cap":         func(l *workspaceLogLimits) { l.maxScanBytes = 0 },
		"long duration":         func(l *workspaceLogLimits) { l.duration = MaxWorkspaceLogDuration + time.Second },
	} {
		limits := defaultWorkspaceLogLimits()
		mutate(&limits)
		if err := limits.validate(); err == nil {
			t.Errorf("%s: invalid limits accepted", name)
		}
	}
}
