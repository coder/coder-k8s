package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

// followFake records what one follow stream saw.
type followFake struct {
	after  atomic.Int64
	closed chan struct{} // closed when the client side of the websocket is gone
}

// serveFollow upgrades the request, sends entries, then calls live with a send function and
// closes the stream when live returns.
func serveFollow(t *testing.T, f *logFakeCoder, entries []codersdk.ProvisionerJobLog, live func(ctx context.Context, send func(codersdk.ProvisionerJobLog) error)) *followFake {
	t.Helper()
	ff := &followFake{closed: make(chan struct{})}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		ff.after.Store(after)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		ctx := conn.CloseRead(r.Context())
		go func() { <-ctx.Done(); close(ff.closed) }()
		send := func(e codersdk.ProvisionerJobLog) error {
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			return conn.Write(ctx, websocket.MessageText, data)
		}
		for _, e := range entries {
			if err := send(e); err != nil {
				return
			}
		}
		if live != nil {
			live(ctx, send)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	f.followHandler.Store(&h)
	return ff
}

func followOptions(limit *int64) *aggregationv1alpha1.CoderWorkspaceLogOptions {
	return &aggregationv1alpha1.CoderWorkspaceLogOptions{Follow: true, LimitBytes: limit}
}

func openFollow(t *testing.T, s *WorkspaceLogStorage, opts *aggregationv1alpha1.CoderWorkspaceLogOptions) io.ReadCloser {
	t.Helper()
	ctx := logRequestContext(t.Context(), "alice", &recordedWarnings{})
	obj, err := s.Get(ctx, logTestName, opts)
	if err != nil {
		t.Fatal(err)
	}
	body, flush, _, err := obj.(interface {
		InputStream(context.Context, string, string) (io.ReadCloser, bool, string, error)
	}).InputStream(ctx, "v1alpha1", "*/*")
	if err != nil {
		t.Fatal(err)
	}
	if !flush {
		t.Fatal("follow responses must flush")
	}
	return body
}

func waitClosed(t *testing.T, ff *followFake, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ff.closed:
	case <-time.After(within):
		t.Fatalf("%s: the Coder log stream was not closed within %s", what, within)
	}
}

// TestWorkspaceLogFollowStreamsLiveEntries: the snapshot is followed by live entries, one at a
// time, continuing after the last snapshot entry, until Coder closes the stream.
func TestWorkspaceLogFollowStreamsLiveEntries(t *testing.T) {
	f := newLogFakeCoder(t)
	entries := testLogEntries(4)
	f.setLogs(entries[:2])
	next := make(chan struct{})
	ff := serveFollow(t, f, entries[2:3], func(ctx context.Context, send func(codersdk.ProvisionerJobLog) error) {
		select {
		case <-next:
			_ = send(entries[3])
		case <-ctx.Done():
		}
	})
	s := newTestLogStorage(t, f, nil)
	body := openFollow(t, s, followOptions(nil))
	defer func() { _ = body.Close() }()

	lines := bufio.NewReader(body)
	for i := 0; i < 3; i++ {
		line, err := lines.ReadString('\n')
		if err != nil || line != entries[i].Text()+"\n" {
			t.Fatalf("line %d: %q err=%v", i, line, err)
		}
	}
	if got := ff.after.Load(); got != entries[1].ID {
		t.Fatalf("follow continued after %d, want the last snapshot entry %d", got, entries[1].ID)
	}
	close(next)
	rest, err := io.ReadAll(lines)
	if err != nil || string(rest) != entries[3].Text()+"\n" {
		t.Fatalf("after the live entry: %q err=%v", rest, err)
	}
}

func TestWorkspaceLogFollowEndsAtBudget(t *testing.T) {
	f := newLogFakeCoder(t)
	entries := testLogEntries(20)
	f.setLogs(entries[:1])
	ff := serveFollow(t, f, entries[1:], func(ctx context.Context, _ func(codersdk.ProvisionerJobLog) error) { <-ctx.Done() })
	s := newTestLogStorage(t, f, nil)
	limit := int64(len(renderedLog(entries[:3])) + 5)
	body := openFollow(t, s, followOptions(&limit))
	data, err := io.ReadAll(body)
	if err != nil || string(data) != renderedLog(entries)[:limit] {
		t.Fatalf("budget: len=%d err=%v", len(data), err)
	}
	waitClosed(t, ff, 5*time.Second, "budget used up")
	_ = body.Close()
	if s.slots.inUse() != 0 {
		t.Fatalf("slot held after the stream ended: %d", s.slots.inUse())
	}
}

// TestWorkspaceLogFollowDeadlineClosesCoderStream: with a client that never reads, the Coder
// stream is closed at the log deadline and nothing more is read from Coder after it.
func TestWorkspaceLogFollowDeadlineClosesCoderStream(t *testing.T) {
	f := newLogFakeCoder(t)
	f.setLogs(nil)
	var sentAfterDeadline atomic.Int32
	deadline := time.Now().Add(500 * time.Millisecond)
	ff := serveFollow(t, f, nil, func(ctx context.Context, send func(codersdk.ProvisionerJobLog) error) {
		for i := int64(1); ctx.Err() == nil; i++ {
			if send(codersdk.ProvisionerJobLog{ID: i, Output: strings.Repeat("x", 1000)}) != nil {
				return
			}
			if time.Now().After(deadline.Add(time.Second)) {
				sentAfterDeadline.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	s := newTestLogStorage(t, f, func(l *workspaceLogLimits) { l.duration, l.followDuration = 500*time.Millisecond, 500*time.Millisecond })
	body := openFollow(t, s, followOptions(nil))
	waitClosed(t, ff, 3*time.Second, "log deadline with a stalled client")
	if n := sentAfterDeadline.Load(); n > 0 {
		t.Fatalf("Coder could still send %d entries a second after the deadline", n)
	}
	_ = body.Close()
}

func TestWorkspaceLogFollowClientCloseClosesCoderStream(t *testing.T) {
	f := newLogFakeCoder(t)
	f.setLogs(testLogEntries(1))
	ff := serveFollow(t, f, nil, func(ctx context.Context, _ func(codersdk.ProvisionerJobLog) error) { <-ctx.Done() })
	s := newTestLogStorage(t, f, nil)
	body := openFollow(t, s, followOptions(nil))
	if _, err := bufio.NewReader(body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	waitClosed(t, ff, 5*time.Second, "client close")
	if s.slots.inUse() != 0 {
		t.Fatalf("slot held after Close: %d", s.slots.inUse())
	}
}

// TestWorkspaceLogFollowStalledDial: a follow handshake that never completes ends at the log
// deadline with an error, and the slot is freed.
func TestWorkspaceLogFollowStalledDial(t *testing.T) {
	f := newLogFakeCoder(t)
	f.setLogs(testLogEntries(1))
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	f.followHandler.Store(&h)
	s := newTestLogStorage(t, f, func(l *workspaceLogLimits) { l.duration, l.followDuration = 300*time.Millisecond, 300*time.Millisecond })
	start := time.Now()
	if _, _, err := readLog(t, s, logTestName, followOptions(nil)); !apierrors.IsTimeout(err) {
		t.Fatalf("a stalled follow dial: err=%v, want 504 timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stalled dial ended after %s", elapsed)
	}
	if s.slots.inUse() != 0 {
		t.Fatalf("slot held after a stalled dial: %d", s.slots.inUse())
	}
}

// TestWorkspaceLogFollowSkipsDialWhenSnapshotIsCut: a snapshot that already used the byte
// budget ends the response without opening a Coder stream.
func TestWorkspaceLogFollowSkipsDialWhenSnapshotIsCut(t *testing.T) {
	f := newLogFakeCoder(t)
	entries := testLogEntries(3)
	f.setLogs(entries)
	var dials atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dials.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	f.followHandler.Store(&h)
	s := newTestLogStorage(t, f, nil)
	limit := int64(10)
	got, _, err := readLog(t, s, logTestName, followOptions(&limit))
	if err != nil || got != renderedLog(entries)[:10] || dials.Load() != 0 {
		t.Fatalf("cut snapshot with follow: got=%q err=%v dials=%d", got, err, dials.Load())
	}
}
