package controller_test

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These self-tests pin the fake's AI Gateway key routes to the Coder facts
// that the CoderAIGatewayKey controller depends on (plan facts F1 to F12 and
// F17).

var fakeAIGatewayKeyFormat = regexp.MustCompile(`^[0-9A-Za-z]{43}$`)

// requireCoderMessage checks the status and the message of a Coder error, and
// returns its validations.
func requireCoderMessage(t *testing.T, err error, status int, message string) []codersdk.ValidationError {
	t.Helper()
	var sdkErr *codersdk.Error
	require.ErrorAs(t, err, &sdkErr)
	require.Equal(t, status, sdkErr.StatusCode())
	require.Equal(t, message, sdkErr.Message)
	return sdkErr.Validations
}

// steppingClock returns a clock that moves one second per call, so keys get
// distinct created_at times.
func steppingClock(start time.Time) func() time.Time {
	var mu sync.Mutex
	next := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		next = next.Add(time.Second)
		return next
	}
}

// F1 to F4, F7 to F9: create answers the plaintext once. List never holds it,
// returns every key oldest first with the heartbeat, and delete is a hard
// delete by ID.
func TestFakeCoderAIGatewayKeyLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	start := time.Date(2026, 10, 13, 10, 0, 0, 123456789, time.UTC)
	f.now = steppingClock(start)
	c := f.client(t, 2*time.Second)

	first, err := c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "ck8s-a-1"})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, first.ID)
	require.Equal(t, "ck8s-a-1", first.Name)
	require.Regexp(t, fakeAIGatewayKeyFormat, first.Key)
	require.Equal(t, first.Key[:11], first.KeyPrefix)
	require.Equal(t, start.Add(time.Second).Truncate(time.Microsecond), first.CreatedAt, "created_at keeps microseconds")
	second, err := c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "ck8s-a-2"})
	require.NoError(t, err)
	require.NotEqual(t, first.KeyPrefix, second.KeyPrefix)

	heartbeat := start.Add(time.Hour)
	f.setAIGatewayKeyHeartbeat(first.ID, heartbeat)
	heartbeat = heartbeat.Truncate(time.Microsecond) // The database clock keeps microseconds.
	keys, err := c.ListAIGatewayKeys(ctx)
	require.NoError(t, err)
	require.Equal(t, []codersdk.AIGatewayKey{
		{ID: first.ID, Name: first.Name, KeyPrefix: first.KeyPrefix, CreatedAt: first.CreatedAt, LastHeartbeatAt: &heartbeat},
		{ID: second.ID, Name: second.Name, KeyPrefix: second.KeyPrefix, CreatedAt: second.CreatedAt},
	}, keys)
	status, body := fakeRawRequest(t, f, http.MethodGet, "/api/v2/ai-gateway/keys", "")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, first.KeyPrefix)
	require.NotContains(t, body, first.Key)
	require.NotContains(t, body, second.Key)

	require.NoError(t, c.DeleteAIGatewayKey(ctx, first.ID))
	requireCoderMessage(t, c.DeleteAIGatewayKey(ctx, first.ID), http.StatusNotFound,
		"Resource not found or you do not have access to this resource")
	status, body = fakeRawRequest(t, f, http.MethodDelete, "/api/v2/ai-gateway/keys/not-a-uuid", "")
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, body, "Invalid key ID.")
	stored := f.aiGatewayKeys()
	require.Len(t, stored, 1, "a delete removes the row")
	require.Equal(t, second.Key, stored[0].plaintext)

	// A hard delete frees the name. The new key gets a new ID and secret.
	again, err := c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "ck8s-a-1"})
	require.NoError(t, err)
	require.NotEqual(t, first.ID, again.ID)
	require.NotEqual(t, first.Key, again.Key)
}

// F5, F6: the name rule and the duplicate answer. Coder answers a duplicate
// name with 400, not 409. The name CHECK allows only lowercase, and Postgres
// checks it before the lower(name) unique index, so an uppercase variant of
// a stored name gets "Invalid key name.".
func TestFakeCoderAIGatewayKeyNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	c := f.client(t, 2*time.Second)

	for _, name := range []string{"Upper", "two--hyphens", "-leading", "trailing-", "with.dot", "with_underscore", strings.Repeat("a", 65)} {
		_, err := c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: name})
		validations := requireCoderMessage(t, err, http.StatusBadRequest, "Invalid key name.")
		require.Len(t, validations, 1, name)
		require.Equal(t, "name", validations[0].Field, name)
	}
	_, err := c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{})
	requireCoderMessage(t, err, http.StatusBadRequest, "Validation failed.")
	status, body := fakeRawRequest(t, f, http.MethodPost, "/api/v2/ai-gateway/keys", "{")
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, body, "Request body must be valid JSON.")
	require.Empty(t, f.aiGatewayKeys())

	_, err = c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: strings.Repeat("a", 64)})
	require.NoError(t, err)
	_, err = c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "gateway"})
	require.NoError(t, err)
	_, err = c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "gateway"})
	requireCoderMessage(t, err, http.StatusBadRequest, "Key name must be unique.")
	_, err = c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "GATEWAY"})
	requireCoderMessage(t, err, http.StatusBadRequest, "Invalid key name.")
	require.Len(t, f.aiGatewayKeys(), 2)
}

// F11, F17: without the Premium feature every route answers 403 before the
// handler runs, and a coderd without the routes answers 404 "Route not found.",
// which differs from the 404 for a missing key.
func TestFakeCoderAIGatewayKeyGates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	c := f.client(t, 2*time.Second)
	existing, err := c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "existing"})
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		set     func(bool)
		status  int
		message string
	}{
		{name: "feature off", set: func(on bool) { f.setAIGatewayFeature(!on) }, status: http.StatusForbidden, message: "AI Gateway is a Premium feature. Contact sales!"},
		{name: "routes missing", set: f.setAIGatewayRoutesMissing, status: http.StatusNotFound, message: "Route not found."},
	} {
		tc.set(true)
		_, err = c.ListAIGatewayKeys(ctx)
		requireCoderMessage(t, err, tc.status, tc.message)
		_, err = c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "blocked"})
		requireCoderMessage(t, err, tc.status, tc.message)
		requireCoderMessage(t, c.DeleteAIGatewayKey(ctx, existing.ID), tc.status, tc.message)
		tc.set(false)
		keys := f.aiGatewayKeys()
		require.Len(t, keys, 1, tc.name)
		require.Equal(t, existing.ID, keys[0].id, tc.name)
	}
}

// F12: a create that commits and answers a 2xx other than 201 with its body
// is an SDK error whose text holds the plaintext. Callers must never log or
// return that error's text.
func TestFakeCoderAIGatewayKeyNon201AnswerLeaksInErrorText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	c := f.client(t, 2*time.Second)

	f.answerNextAIGatewayKeyCreateWith(http.StatusOK)
	_, err := c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "ck8s-a-1"})
	var sdkErr *codersdk.Error
	require.ErrorAs(t, err, &sdkErr)
	require.Equal(t, http.StatusOK, sdkErr.StatusCode())
	keys := f.aiGatewayKeys()
	require.Len(t, keys, 1, "the key is committed")
	require.Contains(t, err.Error(), keys[0].plaintext)
	require.NotContains(t, sdkErr.Message, keys[0].plaintext)

	// The switch is used up.
	_, err = c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "ck8s-a-2"})
	require.NoError(t, err)
}

// The shared fault injection reaches the create route, so the controller
// tests can lose an answer before or after Coder commits the key.
func TestFakeCoderAIGatewayKeyFaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		fault     fakeFault
		status    int // zero: a transport error without a Coder status
		committed bool
	}{
		{name: "gateway timeout after commit", fault: fakeFault{Status: http.StatusGatewayTimeout, AfterCommit: true}, status: http.StatusGatewayTimeout, committed: true},
		{name: "rate limited", fault: fakeFault{Status: http.StatusTooManyRequests}, status: http.StatusTooManyRequests},
		{name: "reset before commit", fault: fakeFault{Reset: true}},
		{name: "reset after commit", fault: fakeFault{Reset: true, AfterCommit: true}, committed: true},
		{name: "hang before commit", fault: fakeFault{Hang: true}},
		{name: "hang after commit", fault: fakeFault{Hang: true, AfterCommit: true}, committed: true},
		{name: "proxy page before commit", fault: fakeFault{Status: http.StatusOK, HTML: "<html>login</html>"}, status: http.StatusOK},
		{name: "proxy page after commit", fault: fakeFault{Status: http.StatusCreated, HTML: "<html>login</html>", AfterCommit: true}, status: http.StatusCreated, committed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newFakeCoder(t)
			c := f.client(t, 500*time.Millisecond)
			f.failNext(routeCreateAIGatewayKey, tc.fault)

			_, err := c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "ck8s-a-1"})
			require.Error(t, err)
			var sdkErr *codersdk.Error
			if tc.status == 0 {
				require.False(t, errors.As(err, &sdkErr), "want a transport error, got %v", err)
			} else {
				requireCoderStatus(t, err, tc.status)
			}
			require.Equal(t, 1, f.requestCount(routeCreateAIGatewayKey))
			want := 0
			if tc.committed {
				want = 1
			}
			require.Len(t, f.aiGatewayKeys(), want)

			// The fault is used up: the next request reaches the handler.
			_, err = c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "ck8s-a-2"})
			require.NoError(t, err)
		})
	}

	// Rewrite: Coder commits the requested key but answers for another name.
	t.Run("rewrite", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		f := newFakeCoder(t)
		c := f.client(t, 2*time.Second)
		f.failNext(routeCreateAIGatewayKey, fakeFault{Rewrite: func(answer any) any {
			a := answer.(codersdk.CreateAIGatewayKeyResponse)
			a.Name = "someone-else"
			return a
		}})
		got, err := c.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: "ck8s-a-1"})
		require.NoError(t, err)
		require.Equal(t, "someone-else", got.Name)
		keys := f.aiGatewayKeys()
		require.Len(t, keys, 1)
		require.Equal(t, "ck8s-a-1", keys[0].name)
	})
}
