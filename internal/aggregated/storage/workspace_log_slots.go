package storage

import (
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	defaultMaxLogStreams        = 64
	defaultMaxLogStreamsPerUser = 4
	// defaultMaxLogScanBytes caps how much is read from Coder per request. Coder caps a job's log
	// output at about 1 MB, and its JSON encoding is about twice the rendered size.
	defaultMaxLogScanBytes = 16 << 20
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
		duration:          MaxWorkspaceLogDuration,
	}
}

func (l workspaceLogLimits) validate() error {
	if l.maxStreams < 1 || l.maxStreamsPerUser < 1 || l.maxStreamsPerUser > l.maxStreams ||
		l.maxScanBytes < 1 || l.maxBytes < 1 || l.duration <= 0 || l.duration > MaxWorkspaceLogDuration {
		return fmt.Errorf("assertion failed: invalid workspace log limits %+v", l)
	}
	return nil
}

// logSlots counts open log requests per server and per user.
type logSlots struct {
	mu      sync.Mutex
	limits  workspaceLogLimits
	total   int
	perUser map[string]int
}

// acquire takes one slot for user without blocking. The returned release frees it exactly once.
func (s *logSlots) acquire(user string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
