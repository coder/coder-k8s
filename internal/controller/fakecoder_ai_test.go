package controller_test

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
)

// AI provider routes of the fake Coder. Citations name files at Coder tag
// v2.37.3, where these routes are byte-identical to v2.37.2 (the Kind image).
// The routes sit behind the API key only (coderd/coderd.go:2088), and the
// fake's one token is a site owner.
const (
	routeAIProviders      = "aiProviders"
	routeCreateAIProvider = "createAIProvider"
	routeAIProvider       = "aiProvider"
	routeUpdateAIProvider = "updateAIProvider"
	routeDeleteAIProvider = "deleteAIProvider"
)

// fakeAIProvider is one ai_providers row with its keys.
type fakeAIProvider struct {
	id                   uuid.UUID
	name, icon, baseURL  string
	providerType         codersdk.AIProviderType
	displayName          string // The stored column. Empty means SQL NULL.
	enabled, deleted     bool
	createdAt, updatedAt time.Time
	keys                 []fakeAIProviderKey // Ordered as Coder returns them.
}

type fakeAIProviderKey struct {
	id        uuid.UUID
	plaintext string
	createdAt time.Time
}

func (f *fakeCoder) registerAIProviderRoutes(mux *http.ServeMux) {
	f.route(mux, "GET /api/v2/ai/providers", routeAIProviders, f.listAIProviders)
	f.route(mux, "POST /api/v2/ai/providers", routeCreateAIProvider, f.createAIProvider)
	f.route(mux, "GET /api/v2/ai/providers/{idOrName}", routeAIProvider, func(r *http.Request) (int, any) {
		p, status, resp := f.lookupAIProvider(r.PathValue("idOrName"))
		if p == nil {
			return status, resp
		}
		return http.StatusOK, p.view()
	})
	f.route(mux, "PATCH /api/v2/ai/providers/{idOrName}", routeUpdateAIProvider, f.updateAIProvider)
	f.route(mux, "DELETE /api/v2/ai/providers/{idOrName}", routeDeleteAIProvider, f.deleteAIProvider)
}

// aiProvider returns the live provider named name, for test assertions on
// what Coder stores, including key plaintext.
func (f *fakeCoder) aiProvider(name string) (fakeAIProvider, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.aiProviders {
		if p.name == name && !p.deleted {
			return *p, true
		}
	}
	return fakeAIProvider{}, false
}

// aiProviderCount counts provider rows, soft-deleted ones included.
func (f *fakeCoder) aiProviderCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.aiProviders)
}

// view mirrors db2sdk.AIProvider (coderd/database/db2sdk/db2sdk.go:52-75):
// an empty display name shows the provider name, and keys are masked.
func (p *fakeAIProvider) view() codersdk.AIProvider {
	keys := make([]codersdk.AIProviderKey, 0, len(p.keys))
	for _, k := range p.keys {
		keys = append(keys, codersdk.AIProviderKey{ID: k.id, Masked: fakeMaskSecret(k.plaintext), CreatedAt: k.createdAt})
	}
	return codersdk.AIProvider{
		ID: p.id, Type: p.providerType, Name: p.name, DisplayName: cmp.Or(p.displayName, p.name),
		Icon: p.icon, Enabled: p.enabled, BaseURL: p.baseURL, APIKeys: keys, CreatedAt: p.createdAt, UpdatedAt: p.updatedAt,
	}
}

// fakeMaskSecret mirrors aibridge/utils/mask.go:6-35.
func fakeMaskSecret(s string) string {
	runes := []rune(s)
	reveal := 0
	switch n := len(runes); {
	case n == 0:
		return ""
	case n >= 20:
		reveal = 4
	case n >= 10:
		reveal = 2
	case n >= 5:
		reveal = 1
	}
	if len(runes) <= reveal*2 {
		return "..."
	}
	return string(runes[:reveal]) + "..." + string(runes[len(runes)-reveal:])
}

// lookupAIProvider mirrors lookupAIProvider and writeAIProviderError
// (coderd/ai_providers.go:548-563, :629-639). Soft-deleted rows are absent
// (coderd/database/queries/ai_providers.sql:1-27). Callers hold f.mu.
func (f *fakeCoder) lookupAIProvider(idOrName string) (*fakeAIProvider, int, any) {
	id, err := uuid.Parse(idOrName)
	if err != nil && !codersdk.AIProviderNameRegex.MatchString(idOrName) {
		status, resp := fakeError(http.StatusBadRequest, "Invalid provider id or name: must be a UUID or match %s.", codersdk.AIProviderNameRegex)
		return nil, status, resp
	}
	for _, p := range f.aiProviders {
		if !p.deleted && ((err == nil && p.id == id) || (err != nil && p.name == idOrName)) {
			return p, 0, nil
		}
	}
	status, resp := fakeError(http.StatusNotFound, "Resource not found or you do not have access to this resource")
	return nil, status, resp
}

// listAIProviders mirrors aiProvidersList (coderd/ai_providers.go:53-99):
// disabled rows included, deleted rows excluded, ordered by name.
func (f *fakeCoder) listAIProviders(*http.Request) (int, any) {
	out := []codersdk.AIProvider{}
	for _, p := range f.aiProviders {
		if !p.deleted {
			out = append(out, p.view())
		}
	}
	slices.SortFunc(out, func(a, b codersdk.AIProvider) int { return cmp.Compare(a.Name, b.Name) })
	return http.StatusOK, out
}

// createAIProvider mirrors aiProvidersCreate (coderd/ai_providers.go:150-261).
// The fake does not model Bedrock or Copilot settings, which the
// CoderAIProvider type enum excludes.
func (f *fakeCoder) createAIProvider(r *http.Request) (int, any) {
	var req codersdk.CreateAIProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return http.StatusBadRequest, codersdk.Response{Message: "Request body must be valid JSON.", Detail: err.Error()}
	}
	if validations := req.Validate(); len(validations) > 0 {
		return http.StatusBadRequest, codersdk.Response{Message: "Invalid AI provider request.", Validations: validations}
	}
	// The unique index covers live rows only (coderd/database/dump.sql:4804).
	for _, p := range f.aiProviders {
		if p.name == req.Name && !p.deleted {
			return http.StatusConflict, codersdk.Response{
				Message: fmt.Sprintf("AI provider %q already exists.", req.Name),
				Detail:  `pq: duplicate key value violates unique constraint "ai_providers_name_unique"`,
			}
		}
	}
	now := f.now().UTC()
	p := &fakeAIProvider{
		id: uuid.New(), name: req.Name, providerType: req.Type, displayName: req.DisplayName, icon: req.Icon,
		enabled: req.Enabled, baseURL: req.BaseURL, createdAt: now, updatedAt: now,
	}
	p.keys = insertFakeAIProviderKeys(nil, req.APIKeys, now)
	f.aiProviders[p.id] = p
	return http.StatusCreated, p.view()
}

// updateAIProvider mirrors aiProvidersUpdate (coderd/ai_providers.go:273-455)
// and applyAIProviderKeyOps (:768-822): an api_keys list is the new key set.
// An {id} entry keeps that key, a plaintext entry always inserts a new key
// with a new ID, and every unlisted key is deleted. The row and its keys
// change together or not at all. Unknown JSON fields such as name and type
// are ignored, because the request type has no such fields
// (codersdk/aiproviders.go:315-322).
func (f *fakeCoder) updateAIProvider(r *http.Request) (int, any) {
	var req codersdk.UpdateAIProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return http.StatusBadRequest, codersdk.Response{Message: "Request body must be valid JSON.", Detail: err.Error()}
	}
	if req.IsEmpty() {
		return fakeError(http.StatusBadRequest, "At least one field must be provided.")
	}
	if validations := req.Validate(); len(validations) > 0 {
		return http.StatusBadRequest, codersdk.Response{Message: "Invalid AI provider request.", Validations: validations}
	}
	p, status, resp := f.lookupAIProvider(r.PathValue("idOrName"))
	if p == nil {
		return status, resp
	}
	now := f.now().UTC()
	keys := p.keys
	if req.APIKeys != nil {
		keep := map[uuid.UUID]bool{}
		var inserts []string
		for _, m := range *req.APIKeys {
			if m.ID == nil {
				inserts = append(inserts, *m.APIKey)
				continue
			}
			if !slices.ContainsFunc(p.keys, func(k fakeAIProviderKey) bool { return k.id == *m.ID }) {
				return http.StatusBadRequest, codersdk.Response{
					Message: "api_keys references an unknown id for this provider",
					Detail:  fmt.Sprintf("execute transaction: api_keys references an unknown id for this provider: %s", *m.ID),
				}
			}
			keep[*m.ID] = true
		}
		kept := slices.DeleteFunc(slices.Clone(p.keys), func(k fakeAIProviderKey) bool { return !keep[k.id] })
		keys = insertFakeAIProviderKeys(kept, inserts, now)
	}
	if req.DisplayName != nil {
		p.displayName = *req.DisplayName // Empty clears the column (coderd/ai_providers.go:363-367).
	}
	p.icon = ptrValue(req.Icon, p.icon)
	p.enabled = ptrValue(req.Enabled, p.enabled)
	p.baseURL = ptrValue(req.BaseURL, p.baseURL)
	p.keys, p.updatedAt = keys, now
	return http.StatusOK, p.view()
}

// deleteAIProvider mirrors aiProvidersDelete (coderd/ai_providers.go:464-500)
// and DeleteAIProviderByID (coderd/database/queries/ai_providers.sql:84-92):
// a soft delete that also disables the row. Coder allows it while chat
// models reference the provider. A second delete finds no live row: 404.
func (f *fakeCoder) deleteAIProvider(r *http.Request) (int, any) {
	p, status, resp := f.lookupAIProvider(r.PathValue("idOrName"))
	if p == nil {
		return status, resp
	}
	p.deleted, p.enabled, p.updatedAt = true, false, f.now().UTC()
	return http.StatusNoContent, nil
}

// insertFakeAIProviderKeys appends one key row with a new ID per plaintext,
// then orders keys by created_at and ID
// (coderd/database/queries/ai_provider_keys.sql:9-22).
func insertFakeAIProviderKeys(keys []fakeAIProviderKey, plaintexts []string, now time.Time) []fakeAIProviderKey {
	for _, plaintext := range plaintexts {
		keys = append(keys, fakeAIProviderKey{id: uuid.New(), plaintext: plaintext, createdAt: now})
	}
	slices.SortStableFunc(keys, func(a, b fakeAIProviderKey) int {
		return cmp.Or(a.createdAt.Compare(b.createdAt), cmp.Compare(a.id.String(), b.id.String()))
	})
	return keys
}

func ptrValue[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}
