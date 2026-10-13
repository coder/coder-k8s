package controller

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const testAIGatewayUID = types.UID("0f8e2a6c-1b3d-4e5f-9a7b-2c4d6e8f0a1b")

// Plan 4.2 steps 6 and 7 as a table: which listed keys belong to the object,
// what the Secret holds, and the action that follows.
func TestNextAIGatewayKeyAction(t *testing.T) {
	t.Parallel()
	prefix, err := aiGatewayKeyNamePrefix(testAIGatewayUID)
	require.NoError(t, err)
	require.Equal(t, "ck8s-0f8e2a6c1b3d4e5f9a7b2c4d6e8f0a1b-", prefix)
	for _, uid := range []types.UID{"", "0F8E2A6C-1B3D-4E5F-9A7B-2C4D6E8F0A1B", "0f8e2a6c-1b3d-4e5f-9a7b"} {
		_, err := aiGatewayKeyNamePrefix(uid)
		require.ErrorContains(t, err, "assertion failed", uid)
	}

	current := codersdk.AIGatewayKey{ID: uuid.New(), Name: prefix + "1", KeyPrefix: "AbCdEfGh012"}
	other := codersdk.AIGatewayKey{ID: uuid.New(), Name: prefix + "2", KeyPrefix: "ZyXwVuTs987"}
	listed := []codersdk.AIGatewayKey{
		current, other,
		// Never this object's: no serial, a non-digit serial, another UID,
		// Carol's manual key.
		{ID: uuid.New(), Name: prefix, KeyPrefix: "Aaaaaaaaaa1"},
		{ID: uuid.New(), Name: prefix + "1a", KeyPrefix: "Aaaaaaaaaa2"},
		{ID: uuid.New(), Name: "ck8s-1f8e2a6c1b3d4e5f9a7b2c4d6e8f0a1b-1", KeyPrefix: "Aaaaaaaaaa3"},
		{ID: uuid.New(), Name: "carol", KeyPrefix: "Aaaaaaaaaa4"},
	}
	own := ownAIGatewayKeys(prefix, listed)
	require.Equal(t, []codersdk.AIGatewayKey{current, other}, own)

	secretWith := func(value string) *corev1.Secret {
		return &corev1.Secret{Data: map[string][]byte{coderv1alpha1.CoderAIGatewayKeySecretDataKey: []byte(value)}}
	}
	tail := strings.Repeat("x", 32)
	for name, tc := range map[string]struct {
		secret *corev1.Secret
		want   uuid.UUID
	}{
		"absent":               {secret: nil, want: uuid.Nil},
		"empty after claim":    {secret: &corev1.Secret{}, want: uuid.Nil},
		"current":              {secret: secretWith("AbCdEfGh012" + tail), want: current.ID},
		"other own key":        {secret: secretWith("ZyXwVuTs987" + tail), want: other.ID},
		"another object's key": {secret: secretWith("Aaaaaaaaaa3" + tail), want: uuid.Nil},
		"too short":            {secret: secretWith("AbCdEfGh012" + tail[1:]), want: uuid.Nil},
		"trailing newline":     {secret: secretWith("AbCdEfGh012" + tail + "\n"), want: uuid.Nil},
		"not alphanumeric":     {secret: secretWith("AbCdEfGh012" + tail[1:] + "-"), want: uuid.Nil},
	} {
		require.Equal(t, tc.want, aiGatewayKeySecretHolds(tc.secret, own), name)
	}

	currentStatus := &coderv1alpha1.CoderAIGatewayKeyCurrent{Serial: 1, ID: current.ID.String(), Name: current.Name}
	for name, tc := range map[string]struct {
		status coderv1alpha1.CoderAIGatewayKeyStatus
		own    []codersdk.AIGatewayKey
		holds  uuid.UUID
		want   aiGatewayKeyAction
	}{
		"new object":                 {want: aiGatewayKeyIssue},
		"new object, orphans listed": {own: own, want: aiGatewayKeyIssue},
		"create in flight":           {status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 1, PendingKey: &coderv1alpha1.CoderAIGatewayKeyPending{}}, want: aiGatewayKeyRecover},
		"in flight during rotation": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus, PendingKey: &coderv1alpha1.CoderAIGatewayKeyPending{Serial: 2}},
			own:    own, holds: current.ID, want: aiGatewayKeyRecover,
		},
		"steady state":         {status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus}, own: own, holds: current.ID, want: aiGatewayKeyReady},
		"current key deleted":  {status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus}, own: own[1:], holds: uuid.Nil, want: aiGatewayKeyKillSwitch},
		"secret emptied":       {status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus}, own: own, holds: uuid.Nil, want: aiGatewayKeyReissue},
		"secret holds another": {status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus}, own: own, holds: other.ID, want: aiGatewayKeyReissue},
	} {
		require.Equal(t, tc.want, nextAIGatewayKeyAction(&tc.status, tc.own, tc.holds), name)
	}
}

// Plan 4.2 step 4: every LIST failure class, with Coder's details left out.
func TestAIGatewayKeyListFailure(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status      int
		message     string
		reason      string
		stalled     bool
		rateLimited bool
	}{
		"no license":      {status: http.StatusForbidden, message: "AI Gateway is a Premium feature. Contact sales!", reason: "LicenseRequired", stalled: true},
		"not site owner":  {status: http.StatusForbidden, message: "Forbidden.", reason: "CoderRejected", stalled: true},
		"route missing":   {status: http.StatusNotFound, message: "Route not found.", reason: "NotSupported", stalled: true},
		"other not found": {status: http.StatusNotFound, message: "Resource not found.", reason: "CoderUnavailable"},
		"token rejected":  {status: http.StatusUnauthorized, message: "You must be logged in.", reason: "CoderUnavailable"},
		"rate limited":    {status: http.StatusTooManyRequests, message: "Rate limit.", reason: "CoderUnavailable", rateLimited: true},
		"server error":    {status: http.StatusBadGateway, message: "Bad gateway.", reason: "CoderUnavailable"},
	} {
		step := aiGatewayKeyListFailure(codersdk.NewError(tc.status, codersdk.Response{Message: tc.message, Detail: "detail-must-not-leak"}))
		require.Equal(t, tc.reason, step.reason, name)
		require.Equal(t, tc.stalled, step.stalled, name)
		require.Equal(t, tc.rateLimited, step.rateLimited, name)
		require.NotContains(t, step.message, "detail-must-not-leak", name)
		require.Contains(t, step.message, tc.message, name)
	}
	step := aiGatewayKeyListFailure(errors.New("dial tcp: connection refused"))
	require.Equal(t, "CoderUnavailable", step.reason)
	require.False(t, step.stalled)
}
