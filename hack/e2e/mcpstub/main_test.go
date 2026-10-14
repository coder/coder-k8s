package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The synthetic keys never appear in a log line or in the report.
const (
	testAPIKey    = "synthetic-api-key-0123456789"
	testHeaderVal = "synthetic-header-abcdef"
)

func sha(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// captureLog sends the standard logger to a buffer for one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

func do(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "mcp-stub.coder.svc:8080"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func rpc(t *testing.T, h http.Handler, body string, headers map[string]string) map[string]any {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/mcp", body, headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /mcp %s: status %d, want 200", body, rec.Code)
	}
	var answer map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer to %s: %v", body, err)
	}
	if answer["jsonrpc"] != "2.0" {
		t.Fatalf("answer to %s: jsonrpc %v, want 2.0", body, answer["jsonrpc"])
	}
	return answer
}

func getReport(t *testing.T, h http.Handler) report {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/report", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /report: status %d", rec.Code)
	}
	var got report
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	return got
}

func TestMCPAnswers(t *testing.T) {
	h := newStub().handler()
	creds := map[string]string{apiKeyHeader: testAPIKey, e2eHeader: testHeaderVal}

	initAnswer := rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`, creds)
	result, _ := initAnswer["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize protocolVersion = %v, want the client's version", result["protocolVersion"])
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("initialize capabilities lack tools: %v", result)
	}

	if rec := do(t, h, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, creds); rec.Code != http.StatusAccepted {
		t.Fatalf("notification: status %d, want 202", rec.Code)
	}

	list := rpc(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, creds)
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "alpha,beta,Gamma" {
		t.Fatalf("tools/list names = %v, want alpha,beta,Gamma", names)
	}

	call := rpc(t, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"Gamma"}}`, creds)
	if !strings.Contains(string(mustJSON(t, call["result"])), "called Gamma") {
		t.Fatalf("tools/call result = %v", call["result"])
	}
	unknown := rpc(t, h, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"gamma"}}`, creds)
	if unknown["error"] == nil {
		t.Fatalf("tools/call of an unknown tool returned no error: %v", unknown)
	}
	if ping := rpc(t, h, `{"jsonrpc":"2.0","id":5,"method":"ping"}`, creds); ping["result"] == nil {
		t.Fatalf("ping returned no result: %v", ping)
	}
	if missing := rpc(t, h, `{"jsonrpc":"2.0","id":6,"method":"resources/list"}`, creds); missing["error"] == nil {
		t.Fatalf("an unknown method returned no error: %v", missing)
	}

	if rec := do(t, h, http.MethodGet, "/mcp", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp: status %d, want 405", rec.Code)
	}
	if rec := do(t, h, http.MethodDelete, "/mcp", "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /mcp: status %d, want 204", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/mcp", "not json", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /mcp with bad JSON: status %d, want 400", rec.Code)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestReportHashesHeadersAndLogsNoValue(t *testing.T) {
	logs := captureLog(t)
	h := newStub().handler()

	rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, map[string]string{apiKeyHeader: testAPIKey, e2eHeader: testHeaderVal})
	rpc(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)

	got := getReport(t, h)
	want := []mcpRecord{
		{Host: "mcp-stub.coder.svc:8080", Method: "initialize", APIKeyHash: sha(testAPIKey), HeaderHash: sha(testHeaderVal)},
		{Host: "mcp-stub.coder.svc:8080", Method: "tools/list"},
	}
	if string(mustJSON(t, got.MCP)) != string(mustJSON(t, want)) {
		t.Fatalf("report MCP = %+v, want %+v", got.MCP, want)
	}

	raw := do(t, h, http.MethodGet, "/report", "", nil).Body.String()
	for _, secret := range []string{testAPIKey, testHeaderVal} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the log holds a header value: %q", logs.String())
		}
		if strings.Contains(raw, secret) {
			t.Fatalf("the report holds a header value: %s", raw)
		}
	}
	if !strings.Contains(logs.String(), "mcp initialize") {
		t.Fatalf("the log lacks the method line: %q", logs.String())
	}
}

func TestChatCompletions(t *testing.T) {
	h := newStub().handler()
	toolsBody := `"tools":[{"type":"function","function":{"name":"srv__beta"}},{"type":"function","function":{"name":"srv__alpha"}}]`

	plain := do(t, h, http.MethodPost, "/v1/chat/completions", `{"model":"stub-model","messages":[],`+toolsBody+`}`, nil)
	if plain.Code != http.StatusOK {
		t.Fatalf("plain chat: status %d", plain.Code)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(plain.Body.Bytes(), &completion); err != nil || len(completion.Choices) != 1 || completion.Choices[0].Message.Content != answerText {
		t.Fatalf("plain chat answer = %s (err %v)", plain.Body.String(), err)
	}

	stream := do(t, h, http.MethodPost, "/v1/chat/completions", `{"model":"stub-model","stream":true,"messages":[]}`, nil)
	if ct := stream.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream Content-Type = %q", ct)
	}
	events := strings.Split(strings.TrimSpace(stream.Body.String()), "\n\n")
	if len(events) != 3 || events[2] != "data: [DONE]" {
		t.Fatalf("stream events = %q, want two chunks and [DONE]", events)
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(events[0], "data: ")), &chunk); err != nil {
		t.Fatalf("decode the first chunk: %v", err)
	}
	if !strings.Contains(events[0], answerText) {
		t.Fatalf("the first chunk lacks the answer: %s", events[0])
	}

	got := getReport(t, h)
	want := []llmRecord{{Stream: false, Tools: []string{"srv__alpha", "srv__beta"}}, {Stream: true, Tools: []string{}}}
	if string(mustJSON(t, got.LLM)) != string(mustJSON(t, want)) {
		t.Fatalf("report LLM = %+v, want %+v", got.LLM, want)
	}

	models := do(t, h, http.MethodGet, "/v1/models", "", nil)
	body, _ := io.ReadAll(models.Body)
	if models.Code != http.StatusOK || !strings.Contains(string(body), `"stub-model"`) {
		t.Fatalf("GET /v1/models: %d %s", models.Code, body)
	}
	if rec := do(t, h, http.MethodGet, "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz: status %d", rec.Code)
	}
}
