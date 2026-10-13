package controller_test

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
)

// AI Gateway key routes of the fake Coder. Citations name files at Coder tag
// v2.37.3. The routes sit behind the API key and then the Premium AI Gateway
// feature (enterprise/coderd/coderd.go:318-328), and the fake's one token is
// a site owner.
const (
	routeAIGatewayKeys           = "aiGatewayKeys"
	routeCreateAIGatewayKey      = "createAIGatewayKey"
	routeDeleteAIGatewayKey      = "deleteAIGatewayKey"
	fakeAIGatewayKeyLength       = 43 // coderd/aibridge/keys/keys.go:11-20
	fakeAIGatewayKeyPrefixLength = 11 // The visible prefix is the key's first 11 characters.
)

// fakeAIGatewayKeyNameRegex is the ai_gateway_keys_name_check constraint
// (coderd/database/dump.sql:1578), together with its 64-character limit.
var fakeAIGatewayKeyNameRegex = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// fakeAIGatewayState is the ai_gateway_keys table and the switches that
// change how the routes answer. Callers hold f.mu.
type fakeAIGatewayState struct {
	keys []*fakeAIGatewayKey // Insert order. Deletes remove rows (hard delete).
	// featureOff answers every route with the Premium feature 403. Coder
	// gives this answer both without a license and with
	// CODER_AI_GATEWAY_ENABLED=false (enterprise/coderd/coderd.go:989).
	featureOff bool
	// routesMissing answers every route like a coderd without them (AGPL or
	// older): 404 "Route not found." (coderd/coderd.go:1416).
	routesMissing bool
	// nextCreateStatus, when set, answers the next successful create with
	// this status instead of 201, still with the full create body. The SDK
	// then reports an error whose text holds the body, plaintext included
	// (codersdk/client.go:455-461).
	nextCreateStatus int
}

// fakeAIGatewayKey is one ai_gateway_keys row (coderd/database/dump.sql:1570-1580).
type fakeAIGatewayKey struct {
	id              uuid.UUID
	name            string
	plaintext       string // Coder stores only the prefix and a SHA-256 hash.
	createdAt       time.Time
	lastHeartbeatAt *time.Time
}

func (k *fakeAIGatewayKey) view() codersdk.AIGatewayKey {
	return codersdk.AIGatewayKey{
		ID: k.id, Name: k.name, KeyPrefix: k.plaintext[:fakeAIGatewayKeyPrefixLength],
		CreatedAt: k.createdAt, LastHeartbeatAt: k.lastHeartbeatAt,
	}
}

func (f *fakeCoder) registerAIGatewayKeyRoutes(mux *http.ServeMux) {
	f.route(mux, "GET /api/v2/ai-gateway/keys", routeAIGatewayKeys, f.aiGatewayGate(f.listAIGatewayKeys))
	f.route(mux, "POST /api/v2/ai-gateway/keys", routeCreateAIGatewayKey, f.aiGatewayGate(f.createAIGatewayKey))
	f.route(mux, "DELETE /api/v2/ai-gateway/keys/{key}", routeDeleteAIGatewayKey, f.aiGatewayGate(f.deleteAIGatewayKey))
}

// aiGatewayGate mirrors what runs before the handlers: a missing route, then
// RequireFeatureMW (enterprise/coderd/templates.go:352-367). The fake checks
// the session token first for every route, which real Coder does after the
// router match. That order does not matter with the fake's one valid token.
func (f *fakeCoder) aiGatewayGate(h func(*http.Request) (int, any)) func(*http.Request) (int, any) {
	return func(r *http.Request) (int, any) {
		switch {
		case f.aiGateway.routesMissing:
			return fakeError(http.StatusNotFound, "Route not found.")
		case f.aiGateway.featureOff:
			return fakeError(http.StatusForbidden, "AI Gateway is a Premium feature. Contact sales!")
		}
		return h(r)
	}
}

// listAIGatewayKeys mirrors aiGatewayKeys (enterprise/coderd/aigatewaykeys.go:120-141)
// and ListAIGatewayKeys (coderd/database/queries/ai_gateway_keys.sql:6-9):
// every key, oldest first, never the secret.
func (f *fakeCoder) listAIGatewayKeys(*http.Request) (int, any) {
	keys := slices.Clone(f.aiGateway.keys)
	slices.SortStableFunc(keys, func(a, b *fakeAIGatewayKey) int { return a.createdAt.Compare(b.createdAt) })
	out := make([]codersdk.AIGatewayKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.view())
	}
	return http.StatusOK, out
}

// createAIGatewayKey mirrors postAIGatewayKey and writeKeyInsertError
// (enterprise/coderd/aigatewaykeys.go:32-111) and httpapi.Read
// (coderd/httpapi/httpapi.go:254-297).
func (f *fakeCoder) createAIGatewayKey(r *http.Request) (int, any) {
	var req codersdk.CreateAIGatewayKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return http.StatusBadRequest, codersdk.Response{Message: "Request body must be valid JSON.", Detail: err.Error()}
	}
	if req.Name == "" {
		return http.StatusBadRequest, codersdk.Response{Message: "Validation failed.", Validations: []codersdk.ValidationError{
			{Field: "name", Detail: `Validation failed for tag "required" with value: ""`},
		}}
	}
	// Postgres checks the name CHECK before the unique index, so a bad name
	// never reaches the duplicate answer below.
	if len(req.Name) > 64 || !fakeAIGatewayKeyNameRegex.MatchString(req.Name) {
		return http.StatusBadRequest, codersdk.Response{Message: "Invalid key name.", Validations: []codersdk.ValidationError{{
			Field:  "name",
			Detail: "Must be 64 characters or fewer, lowercase letters, numbers, and non-consecutive hyphens, cannot start or end with a hyphen.",
		}}}
	}
	// The unique index is on lower(name) over all rows (coderd/database/dump.sql:4800),
	// and Coder answers a duplicate with 400, not 409, also under a race.
	if slices.ContainsFunc(f.aiGateway.keys, func(k *fakeAIGatewayKey) bool { return strings.EqualFold(k.name, req.Name) }) {
		return http.StatusBadRequest, codersdk.Response{Message: "Key name must be unique.", Validations: []codersdk.ValidationError{
			{Field: "name", Detail: "A key with this name already exists."},
		}}
	}
	k := &fakeAIGatewayKey{
		id: uuid.New(), name: req.Name, plaintext: f.newAIGatewayKeySecret(),
		// created_at is the database clock (NOW()), which keeps microseconds
		// (coderd/database/queries/ai_gateway_keys.sql:1-4).
		createdAt: f.now().UTC().Truncate(time.Microsecond),
	}
	f.aiGateway.keys = append(f.aiGateway.keys, k)
	status := http.StatusCreated
	if f.aiGateway.nextCreateStatus != 0 {
		status, f.aiGateway.nextCreateStatus = f.aiGateway.nextCreateStatus, 0
	}
	return status, codersdk.CreateAIGatewayKeyResponse{
		ID: k.id, Name: k.name, Key: k.plaintext, KeyPrefix: k.plaintext[:fakeAIGatewayKeyPrefixLength], CreatedAt: k.createdAt,
	}
}

// newAIGatewayKeySecret returns 43 random alphanumeric characters
// (coderd/aibridge/keys/keys.go:24-43, cryptorand/strings.go:26) whose
// 11-character prefix no stored key has: the prefix and the hash are unique
// indexes (coderd/database/dump.sql:4798, :4802). Callers hold f.mu.
func (f *fakeCoder) newAIGatewayKeySecret() string {
	const charset = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for {
		b := make([]byte, fakeAIGatewayKeyLength)
		if _, err := rand.Read(b); err != nil {
			panic(fmt.Sprintf("assertion failed: read random bytes: %v", err))
		}
		for i := range b {
			b[i] = charset[int(b[i])%len(charset)]
		}
		secret := string(b)
		if !slices.ContainsFunc(f.aiGateway.keys, func(k *fakeAIGatewayKey) bool {
			return k.plaintext[:fakeAIGatewayKeyPrefixLength] == secret[:fakeAIGatewayKeyPrefixLength]
		}) {
			return secret
		}
	}
}

// deleteAIGatewayKey mirrors deleteAIGatewayKey (enterprise/coderd/aigatewaykeys.go:150-197)
// and DeleteAIGatewayKey (coderd/database/queries/ai_gateway_keys.sql:11-13):
// a hard delete by ID. A missing ID answers httpapi.ResourceNotFound.
func (f *fakeCoder) deleteAIGatewayKey(r *http.Request) (int, any) {
	id, err := uuid.Parse(r.PathValue("key"))
	if err != nil {
		return http.StatusBadRequest, codersdk.Response{Message: "Invalid key ID.", Detail: err.Error()}
	}
	i := slices.IndexFunc(f.aiGateway.keys, func(k *fakeAIGatewayKey) bool { return k.id == id })
	if i < 0 {
		return fakeError(http.StatusNotFound, "Resource not found or you do not have access to this resource")
	}
	f.aiGateway.keys = slices.Delete(f.aiGateway.keys, i, i+1)
	return http.StatusNoContent, nil
}

// Setup helpers and assertions. They lock the fake.

// aiGatewayKeys returns copies of the stored keys in insert order, plaintext
// included, for assertions on what Coder holds.
func (f *fakeCoder) aiGatewayKeys() []fakeAIGatewayKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeAIGatewayKey, 0, len(f.aiGateway.keys))
	for _, k := range f.aiGateway.keys {
		out = append(out, *k)
	}
	return out
}

// setAIGatewayKeyHeartbeat records a heartbeat for the key, as an AI Gateway
// connection does every 60 s (enterprise/coderd/aibridgeserve.go:29-31, :204-216).
func (f *fakeCoder) setAIGatewayKeyHeartbeat(id uuid.UUID, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.aiGateway.keys {
		if k.id == id {
			at = at.UTC().Truncate(time.Microsecond)
			k.lastHeartbeatAt = &at
			return
		}
	}
	f.t.Fatalf("assertion failed: fake coder has no AI Gateway key %s", id)
}

// setAIGatewayFeature turns the Premium AI Gateway feature on or off.
func (f *fakeCoder) setAIGatewayFeature(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aiGateway.featureOff = !enabled
}

// setAIGatewayRoutesMissing makes the fake answer like a coderd without the
// key routes.
func (f *fakeCoder) setAIGatewayRoutesMissing(missing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aiGateway.routesMissing = missing
}

// answerNextAIGatewayKeyCreateWith makes the next successful create commit
// and answer status (for example 200) with the full create body.
func (f *fakeCoder) answerNextAIGatewayKeyCreateWith(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aiGateway.nextCreateStatus = status
}
