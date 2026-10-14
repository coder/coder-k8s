// Command mcpstub is the Kind E2E stub of the CoderMCPServer plan (#155,
// section 7.2). It serves a minimal MCP server (streamable HTTP with JSON
// answers) on /mcp, a minimal OpenAI-compatible LLM on /v1, and /report,
// which lists for each MCP request the Host header and SHA-256 hashes of
// two credential headers. It never logs or reports a header value.
//
// It uses only the standard library, so go.mod and vendor/ do not change.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Header names whose values the report hashes. The E2E driver writes the
// matching synthetic keys into credential Secrets.
const (
	apiKeyHeader = "X-Api-Key" //nolint:gosec // G101: a header name, not a credential.
	e2eHeader    = "X-E2E-Header"
	// maxBody bounds a request body. Coder sends small JSON-RPC requests.
	maxBody = 1 << 20
)

// toolNames are the stub's MCP tools. Gamma checks that tool names keep
// their case.
var toolNames = []string{"alpha", "beta", "Gamma"}

// mcpRecord is one MCP request in the report. Hashes are hex SHA-256 of the
// header value, or "" when the header is absent.
type mcpRecord struct {
	Host       string `json:"host"`
	Method     string `json:"method"`
	APIKeyHash string `json:"apiKeyHash"`
	HeaderHash string `json:"headerHash"`
}

// llmRecord is one LLM request in the report: the tool names that Coder
// offered the model, sorted.
type llmRecord struct {
	Stream bool     `json:"stream"`
	Tools  []string `json:"tools"`
}

type report struct {
	MCP []mcpRecord `json:"mcp"`
	LLM []llmRecord `json:"llm"`
}

// stub holds the report. It is safe for concurrent use.
type stub struct {
	mu     sync.Mutex
	report report
	now    func() time.Time
}

func newStub() *stub {
	return &stub{report: report{MCP: []mcpRecord{}, LLM: []llmRecord{}}, now: time.Now}
}

func hashHeader(r *http.Request, name string) string {
	v := r.Header.Get(name)
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func (s *stub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.mcp)
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /report", s.serveReport)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (s *stub) mcp(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// No server-sent event stream: Coder then uses the POST answers.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.report.MCP = append(s.report.MCP, mcpRecord{
		Host: r.Host, Method: req.Method, APIKeyHash: hashHeader(r, apiKeyHeader), HeaderHash: hashHeader(r, e2eHeader),
	})
	s.mu.Unlock()
	log.Printf("mcp %s", req.Method)
	if len(req.ID) == 0 {
		// A notification, such as notifications/initialized.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := mcpResult(req)
	answer := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rpcErr != nil {
		answer["error"] = rpcErr
	} else {
		answer["result"] = result
	}
	w.Header().Set("Mcp-Session-Id", "mcpstub-session")
	writeJSON(w, answer)
}

func mcpResult(req rpcRequest) (any, map[string]any) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mcpstub", "version": "1"},
		}, nil
	case "tools/list":
		tools := make([]map[string]any, 0, len(toolNames))
		for _, name := range toolNames {
			tools = append(tools, map[string]any{
				"name": name, "description": "tool " + name,
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			})
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if !slices.Contains(toolNames, p.Name) {
			return nil, map[string]any{"code": -32602, "message": "unknown tool"}
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": "called " + p.Name}}}, nil
	case "ping":
		return map[string]any{}, nil
	}
	return nil, map[string]any{"code": -32601, "message": "method not found"}
}

const answerText = "stub answer"

func (s *stub) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil {
		http.Error(w, "invalid chat request", http.StatusBadRequest)
		return
	}
	tools := []string{}
	for _, t := range req.Tools {
		tools = append(tools, cmpOr(t.Function.Name, t.Name))
	}
	slices.Sort(tools)
	s.mu.Lock()
	s.report.LLM = append(s.report.LLM, llmRecord{Stream: req.Stream, Tools: tools})
	s.mu.Unlock()
	log.Printf("llm stream=%v tools=%d", req.Stream, len(tools))
	created := s.now().Unix()
	usage := map[string]any{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}
	if !req.Stream {
		writeJSON(w, map[string]any{
			"id": "c1", "object": "chat.completion", "created": created, "model": "stub-model",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": answerText}, "finish_reason": "stop"}},
			"usage":   usage,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	chunks := []map[string]any{
		{
			"id": "c1", "object": "chat.completion.chunk", "created": created, "model": "stub-model",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": answerText}, "finish_reason": nil}},
		},
		{
			"id": "c1", "object": "chat.completion.chunk", "created": created, "model": "stub-model",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage,
		},
	}
	flusher, _ := w.(http.Flusher)
	for _, c := range chunks {
		data, _ := json.Marshal(c)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func (s *stub) models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"object": "list", "data": []map[string]any{{"id": "stub-model", "object": "model", "owned_by": "mcpstub"}}})
}

func (s *stub) serveReport(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.report)
}

func writeJSON(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode answer", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: newStub().handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("mcpstub listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
