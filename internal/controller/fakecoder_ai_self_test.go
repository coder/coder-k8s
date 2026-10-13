package controller_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These self-tests pin the fake's AI provider routes to the Coder facts that
// the CoderAIProvider controller depends on (plan facts F3 to F8 and F21).

const fakeTestAPIKey = "sk-test-0123456789abcdefWXYZ" // 28 characters.

func createFakeAIProvider(ctx context.Context, t *testing.T, c *codersdk.Client, name string, keys ...string) codersdk.AIProvider {
	t.Helper()
	p, err := c.CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderTypeOpenAI, Name: name, BaseURL: "https://api.openai.com/v1", APIKeys: keys,
	})
	require.NoError(t, err)
	return p
}

// fakeRawRequest sends body as is, to check what the SDK cannot send and
// what the raw answer holds.
func fakeRawRequest(t *testing.T, f *fakeCoder, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set(codersdk.SessionTokenHeader, fakeCoderToken)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, string(raw)
}

// F3: keys are write-only. Answers carry an ID and a mask, never the key.
// F7: display_name is free text, and an empty one shows the provider name.
func TestFakeCoderAIProviderReadback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	c := f.client(t, 2*time.Second)

	nonce := "coder-k8s:" + uuid.NewString()
	created, err := c.CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderTypeOpenAI, Name: "openai", DisplayName: nonce, BaseURL: "https://api.openai.com/v1",
		APIKeys: []string{fakeTestAPIKey},
	})
	require.NoError(t, err)
	require.False(t, created.Enabled)
	require.Equal(t, nonce, created.DisplayName)
	require.Len(t, created.APIKeys, 1)
	require.Equal(t, "sk-t...WXYZ", created.APIKeys[0].Masked)

	for _, ident := range []string{"openai", created.ID.String()} {
		got, err := c.AIProvider(ctx, ident)
		require.NoError(t, err)
		require.Equal(t, created.ID, got.ID)
		require.Equal(t, created.APIKeys, got.APIKeys)
	}
	for _, path := range []string{"/api/v2/ai/providers", "/api/v2/ai/providers/openai"} {
		status, body := fakeRawRequest(t, f, http.MethodGet, path, "")
		require.Equal(t, http.StatusOK, status)
		require.NotContains(t, body, fakeTestAPIKey)
	}
	stored, ok := f.aiProvider("openai")
	require.True(t, ok)
	require.Equal(t, fakeTestAPIKey, stored.keys[0].plaintext)

	for key, masked := range map[string]string{"0123456789abcdefghij": "0123...ghij", "0123456789": "01...89", "01234": "0...4", "0123": "..."} {
		require.Equal(t, masked, fakeMaskSecret(key), key)
	}

	// An empty display name falls back to the name, on create and on PATCH.
	plain := createFakeAIProvider(ctx, t, c, "anthropic")
	require.Equal(t, "anthropic", plain.DisplayName)
	cleared, err := c.UpdateAIProvider(ctx, "openai", codersdk.UpdateAIProviderRequest{DisplayName: ptrTo("")})
	require.NoError(t, err)
	require.Equal(t, "openai", cleared.DisplayName)

	// The list holds disabled providers, ordered by name.
	list, err := c.AIProviders(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, []string{"anthropic", "openai"}, []string{list[0].Name, list[1].Name})
}

// F4: a resent key gets a new ID, {id} keeps a key, unlisted keys go, and
// the PATCH applies the URL and keys together.
func TestFakeCoderAIProviderKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	c := f.client(t, 2*time.Second)
	created := createFakeAIProvider(ctx, t, c, "openai", fakeTestAPIKey)
	firstID := created.APIKeys[0].ID

	resent, err := c.UpdateAIProvider(ctx, "openai", codersdk.UpdateAIProviderRequest{
		APIKeys: &[]codersdk.AIProviderKeyMutation{{APIKey: ptrTo(fakeTestAPIKey)}},
	})
	require.NoError(t, err)
	require.Len(t, resent.APIKeys, 1)
	require.NotEqual(t, firstID, resent.APIKeys[0].ID, "the same key sent again is a new key")

	secondID := resent.APIKeys[0].ID
	kept, err := c.UpdateAIProvider(ctx, "openai", codersdk.UpdateAIProviderRequest{
		BaseURL: ptrTo("https://proxy.example.com/v1"),
		APIKeys: &[]codersdk.AIProviderKeyMutation{{ID: &secondID}, {APIKey: ptrTo("sk-second-key-0123456789")}},
	})
	require.NoError(t, err)
	require.Equal(t, "https://proxy.example.com/v1", kept.BaseURL)
	require.Len(t, kept.APIKeys, 2)
	require.Contains(t, []uuid.UUID{kept.APIKeys[0].ID, kept.APIKeys[1].ID}, secondID)

	// A PATCH without api_keys leaves the keys alone.
	untouched, err := c.UpdateAIProvider(ctx, "openai", codersdk.UpdateAIProviderRequest{Enabled: ptrTo(true)})
	require.NoError(t, err)
	require.Equal(t, kept.APIKeys, untouched.APIKeys)
	require.True(t, untouched.Enabled)

	// An unknown ID fails the whole PATCH: URL and keys stay unchanged.
	_, err = c.UpdateAIProvider(ctx, "openai", codersdk.UpdateAIProviderRequest{
		BaseURL: ptrTo("https://other.example.com/v1"),
		APIKeys: &[]codersdk.AIProviderKeyMutation{{ID: ptrTo(uuid.New())}},
	})
	requireCoderStatus(t, err, http.StatusBadRequest)
	after, err := c.AIProvider(ctx, "openai")
	require.NoError(t, err)
	require.Equal(t, untouched.APIKeys, after.APIKeys)
	require.Equal(t, "https://proxy.example.com/v1", after.BaseURL)

	cleared, err := c.UpdateAIProvider(ctx, "openai", codersdk.UpdateAIProviderRequest{APIKeys: &[]codersdk.AIProviderKeyMutation{}})
	require.NoError(t, err)
	require.Empty(t, cleared.APIKeys)
}

// The create answer lists keys in request order. Every later read sorts
// keys that share a creation time by ID.
func TestFakeCoderAIProviderKeyOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	c := f.client(t, 2*time.Second)
	// Six keys: random IDs come out sorted by chance in 1 of 720 runs only.
	var plaintexts, wantMasks []string
	for _, letter := range "zamqbx" {
		plaintexts = append(plaintexts, "sk-"+string(letter)+"-0123456789abcdef")
		wantMasks = append(wantMasks, "sk-"+string(letter)+"...cdef")
	}
	created := createFakeAIProvider(ctx, t, c, "openai", plaintexts...)
	var masks []string
	for _, k := range created.APIKeys {
		masks = append(masks, k.Masked)
	}
	require.Equal(t, wantMasks, masks)

	got, err := c.AIProvider(ctx, "openai")
	require.NoError(t, err)
	require.ElementsMatch(t, created.APIKeys, got.APIKeys)
	require.True(t, slices.IsSortedFunc(got.APIKeys, func(a, b codersdk.AIProviderKey) int {
		return strings.Compare(a.ID.String(), b.ID.String())
	}), "reads order keys of one creation time by ID")
}

// F5: names follow Coder's pattern and are unique among live providers.
// F6: PATCH cannot change name or type.
func TestFakeCoderAIProviderNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	c := f.client(t, 2*time.Second)
	created := createFakeAIProvider(ctx, t, c, "openai")

	_, err := c.CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{Type: codersdk.AIProviderTypeAnthropic, Name: "openai", BaseURL: "https://api.anthropic.com"})
	requireCoderStatus(t, err, http.StatusConflict)
	require.Equal(t, 1, f.aiProviderCount())

	for _, name := range []string{"OpenAI", "open--ai", "open_ai", ""} {
		_, err := c.CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{Type: codersdk.AIProviderTypeOpenAI, Name: name, BaseURL: "https://api.openai.com/v1"})
		requireCoderStatus(t, err, http.StatusBadRequest)
	}
	_, err = c.AIProvider(ctx, "OpenAI")
	requireCoderStatus(t, err, http.StatusBadRequest)
	require.Equal(t, 1, f.aiProviderCount())

	status, _ := fakeRawRequest(t, f, http.MethodPatch, "/api/v2/ai/providers/openai", `{"name":"renamed","type":"anthropic"}`)
	require.Equal(t, http.StatusBadRequest, status, "a PATCH with only name and type has no known field")
	status, body := fakeRawRequest(t, f, http.MethodPatch, "/api/v2/ai/providers/openai", `{"name":"renamed","type":"anthropic","enabled":true}`)
	require.Equal(t, http.StatusOK, status)
	var patched codersdk.AIProvider
	require.NoError(t, json.Unmarshal([]byte(body), &patched))
	require.Equal(t, created.ID, patched.ID)
	require.Equal(t, "openai", patched.Name)
	require.Equal(t, codersdk.AIProviderTypeOpenAI, patched.Type)
	require.True(t, patched.Enabled)
}

// F8: delete is soft. The ID stays gone, and the name can be reused with a
// new ID.
func TestFakeCoderAIProviderSoftDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	c := f.client(t, 2*time.Second)
	old := createFakeAIProvider(ctx, t, c, "openai", fakeTestAPIKey)

	require.NoError(t, c.DeleteAIProvider(ctx, old.ID.String()))
	for _, ident := range []string{"openai", old.ID.String()} {
		_, err := c.AIProvider(ctx, ident)
		requireCoderStatus(t, err, http.StatusNotFound)
		_, err = c.UpdateAIProvider(ctx, ident, codersdk.UpdateAIProviderRequest{Enabled: ptrTo(true)})
		requireCoderStatus(t, err, http.StatusNotFound)
		requireCoderStatus(t, c.DeleteAIProvider(ctx, ident), http.StatusNotFound)
	}
	list, err := c.AIProviders(ctx)
	require.NoError(t, err)
	require.Empty(t, list)

	reused := createFakeAIProvider(ctx, t, c, "openai")
	require.NotEqual(t, old.ID, reused.ID)
	_, err = c.AIProvider(ctx, old.ID.String())
	requireCoderStatus(t, err, http.StatusNotFound)
	require.Equal(t, 2, f.aiProviderCount(), "the soft-deleted row stays")
}

// F21: validation errors name the field, never the value.
func TestFakeCoderAIProviderValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	c := f.client(t, 2*time.Second)
	padded := " " + fakeTestAPIKey

	requireFieldError := func(err error, field string) {
		t.Helper()
		requireCoderStatus(t, err, http.StatusBadRequest)
		var sdkErr *codersdk.Error
		require.ErrorAs(t, err, &sdkErr)
		require.Len(t, sdkErr.Validations, 1)
		require.Equal(t, field, sdkErr.Validations[0].Field)
		require.NotContains(t, err.Error(), strings.TrimSpace(padded))
	}
	_, err := c.CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderTypeOpenAI, Name: "openai", BaseURL: "https://api.openai.com/v1", APIKeys: []string{padded},
	})
	requireFieldError(err, "api_keys[0]")
	createFakeAIProvider(ctx, t, c, "openai")
	_, err = c.UpdateAIProvider(ctx, "openai", codersdk.UpdateAIProviderRequest{APIKeys: &[]codersdk.AIProviderKeyMutation{{APIKey: &padded}}})
	requireFieldError(err, "api_keys[0].api_key")
	for _, baseURL := range []string{"api.openai.com", "ftp://api.openai.com", "https://api.example.com/%zz#x"} {
		_, err = c.UpdateAIProvider(ctx, "openai", codersdk.UpdateAIProviderRequest{BaseURL: &baseURL})
		requireFieldError(err, "base_url")
	}
	_, err = c.UpdateAIProvider(ctx, "openai", codersdk.UpdateAIProviderRequest{})
	requireCoderStatus(t, err, http.StatusBadRequest)
}

// The shared fault injection reaches the provider routes: a create that
// commits before its answer is lost leaves exactly one provider.
func TestFakeCoderAIProviderFaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	c := f.client(t, 2*time.Second)
	f.failNext(routeCreateAIProvider, fakeFault{AfterCommit: true, Reset: true})
	_, err := c.CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{Type: codersdk.AIProviderTypeOpenAI, Name: "openai", BaseURL: "https://api.openai.com/v1"})
	require.Error(t, err)
	_, ok := f.aiProvider("openai")
	require.True(t, ok)
	require.Equal(t, 1, f.requestCount(routeCreateAIProvider))
	_, err = c.CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{Type: codersdk.AIProviderTypeOpenAI, Name: "openai", BaseURL: "https://api.openai.com/v1"})
	requireCoderStatus(t, err, http.StatusConflict)
}
