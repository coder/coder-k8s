package storage

import (
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	// defaultMaxLogStreams caps open log requests per server, and with them the open Coder
	// connections and the memory that log requests hold. The read and write caps bound input and
	// output bytes, not heap: decoding one entry that fills the read cap also costs the decoder's
	// buffer, the decoded strings and the rendered line. BenchmarkWorkspaceLogSingleHugeEntry
	// measures about 24 MiB allocated per request (6x the 4 MiB read cap), so 64 slots stay below
	// about 1.5 GiB in that worst case.
	defaultMaxLogStreams = 64
	// defaultMaxLogStreamsPerUser caps open log requests per user, so one user (for example a
	// script that follows many workspaces) can take at most 1/16 of the server's slots.
	defaultMaxLogStreamsPerUser = 4
	// defaultMaxLogScanBytes caps how much is read from Coder per request. Coder caps a job's log
	// output at about 1 MB. In Kind, a capped build's log was 2.41 MB of JSON. A larger response is
	// cut at this cap with a warning.
	defaultMaxLogScanBytes = 4 << 20
	// defaultMaxLogBytes caps how much one response writes.
	defaultMaxLogBytes = 4 << 20
	// logSlotRetryAfterSeconds is the Retry-After hint when every log slot is taken.
	logSlotRetryAfterSeconds = 5
)

// workspaceLogLimits bounds coderworkspaces/log requests. Production uses
// defaultWorkspaceLogLimits; tests shrink the values through newWorkspaceLogStorage.
type workspaceLogLimits struct {
	maxStreams        int
	maxStreamsPerUser int
	maxScanBytes      int64
	maxBytes          int64
	duration          time.Duration
}

func defaultWorkspaceLogLimits() workspaceLogLimits {
	return workspaceLogLimits{
		maxStreams:        defaultMaxLogStreams,
		maxStreamsPerUser: defaultMaxLogStreamsPerUser,
		maxScanBytes:      defaultMaxLogScanBytes,
		maxBytes:          defaultMaxLogBytes,
		duration:          MaxWorkspaceLogSnapshotDuration,
	}
}

func (l workspaceLogLimits) validate() error {
	if l.maxStreams < 1 || l.maxStreamsPerUser < 1 || l.maxStreamsPerUser > l.maxStreams ||
		l.maxScanBytes < 1 || l.maxBytes < 1 || l.duration <= 0 || l.duration > MaxWorkspaceLogDuration {
		return fmt.Errorf("assertion failed: invalid workspace log limits %+v", l)
	}
	return nil
}

// logSlots counts open log requests per server and per user. Create it with newLogSlots: a
// zero-value logSlots refuses every acquire.
type logSlots struct {
	mu      sync.Mutex
	limits  workspaceLogLimits
	total   int
	perUser map[string]int
}

// newLogSlots returns slots for limits. It panics on invalid limits.
func newLogSlots(limits workspaceLogLimits) *logSlots {
	if err := limits.validate(); err != nil {
		panic(err.Error())
	}
	return &logSlots{limits: limits, perUser: map[string]int{}}
}

// acquire takes one slot for user without blocking. The returned release frees it exactly once.
// user is the authenticated user name after impersonation. It must not be empty, so that
// anonymous or unidentified callers never share one bucket.
func (s *logSlots) acquire(user string) (func(), error) {
	if user == "" {
		return nil, fmt.Errorf("assertion failed: log slot user must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perUser == nil {
		return nil, fmt.Errorf("assertion failed: log slots must be created with newLogSlots")
	}
	if s.total >= s.limits.maxStreams {
		return nil, apierrors.NewTooManyRequests("too many open coderworkspaces/log requests on this server; retry later", logSlotRetryAfterSeconds)
	}
	if s.perUser[user] >= s.limits.maxStreamsPerUser {
		return nil, apierrors.NewTooManyRequests(fmt.Sprintf(
			"too many open coderworkspaces/log requests for this user (limit %d); close one or retry later", s.limits.maxStreamsPerUser,
		), logSlotRetryAfterSeconds)
	}
	s.total++
	s.perUser[user]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.total < 1 || s.perUser[user] < 1 {
				panic("assertion failed: log slot released more often than acquired")
			}
			s.total--
			s.perUser[user]--
			if s.perUser[user] == 0 {
				delete(s.perUser, user)
			}
		})
	}, nil
}

// inUse reports the open slots; tests use it to prove that slots are released.
func (s *logSlots) inUse() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}
