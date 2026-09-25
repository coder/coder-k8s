package mcpapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultHTTPAddr is the listen address used by MCP HTTP mode. It is loopback-only on purpose:
	// the server acts with its ServiceAccount's authority, so it must never listen on the Pod network.
	DefaultHTTPAddr = "127.0.0.1:8090"
	// MinTokenLength is the minimum accepted length of the MCP bearer token, in bytes.
	MinTokenLength = 32
	// streamableHTTPSessionTimeout ensures abandoned MCP streamable HTTP sessions are reclaimed.
	streamableHTTPSessionTimeout = 15 * time.Minute
)

var setupLog = ctrl.Log.WithName("setup")

// newMCPHTTPHandler keeps the production transport options shared with security tests.
func newMCPHTTPHandler(server *mcp.Server) *mcp.StreamableHTTPHandler {
	if server == nil {
		panic("assertion failed: MCP server must not be nil")
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		SessionTimeout: streamableHTTPSessionTimeout,
	})
}

// newMCPHTTPMux builds the production HTTP routing. Only the exact health paths are unauthenticated;
// every other request, including every /mcp request that carries an existing session ID, must present
// the bearer token.
func newMCPHTTPMux(mcpHandler http.Handler, token []byte) http.Handler {
	if mcpHandler == nil {
		panic("assertion failed: MCP handler must not be nil")
	}
	if len(token) < MinTokenLength {
		panic("assertion failed: MCP bearer token must be validated before building the handler")
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	health := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}

	tokenDigest := sha256.Sum256(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			health(w, r)
			return
		}
		if !bearerTokenMatches(r.Header.Get("Authorization"), tokenDigest) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="coder-k8s-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// bearerTokenMatches compares SHA-256 digests in constant time so neither the token bytes nor its
// length leak through timing.
func bearerTokenMatches(header string, want [sha256.Size]byte) bool {
	scheme, credential, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || credential == "" {
		return false
	}
	got := sha256.Sum256([]byte(credential))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// ReadTokenFile reads and validates the MCP bearer token file.
func ReadTokenFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("--mcp-token-file is required for --app=mcp-http")
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the operator-supplied --mcp-token-file flag.
	if err != nil {
		return nil, fmt.Errorf("read --mcp-token-file: %w", err)
	}
	token := bytes.TrimSuffix(bytes.TrimSuffix(data, []byte("\n")), []byte("\r"))
	if len(token) == 0 {
		return nil, fmt.Errorf("--mcp-token-file %q is empty", path)
	}
	if len(token) < MinTokenLength {
		return nil, fmt.Errorf("--mcp-token-file %q holds %d bytes; at least %d are required", path, len(token), MinTokenLength)
	}
	if bytes.ContainsAny(token, " \t\r\n") {
		return nil, fmt.Errorf("--mcp-token-file %q must hold a single token without whitespace", path)
	}
	return token, nil
}

// RunHTTP starts the MCP server using streamable HTTP transport.
func RunHTTP(ctx context.Context, tokenFile string) error {
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}

	// Validate the credential before touching the cluster so a bad token fails fast.
	token, err := ReadTokenFile(tokenFile)
	if err != nil {
		return err
	}

	k8sClient, clientset, err := newClients()
	if err != nil {
		return err
	}

	return RunHTTPWithClients(ctx, k8sClient, clientset, token)
}

// RunHTTPWithClients starts the MCP server using streamable HTTP transport and the provided Kubernetes clients.
func RunHTTPWithClients(ctx context.Context, k8sClient client.Client, clientset kubernetes.Interface, token []byte) error {
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}
	if k8sClient == nil {
		return fmt.Errorf("assertion failed: Kubernetes client must not be nil")
	}
	if clientset == nil {
		return fmt.Errorf("assertion failed: Kubernetes clientset must not be nil")
	}
	if len(token) < MinTokenLength {
		return fmt.Errorf("assertion failed: MCP bearer token must hold at least %d bytes", MinTokenLength)
	}

	server := NewServer(k8sClient, clientset)
	if server == nil {
		return fmt.Errorf("assertion failed: MCP server is nil after successful construction")
	}

	mcpHandler := newMCPHTTPHandler(server)
	if mcpHandler == nil {
		return fmt.Errorf("assertion failed: MCP HTTP handler is nil after successful construction")
	}

	httpServer := &http.Server{
		Addr:              DefaultHTTPAddr,
		Handler:           newMCPHTTPMux(mcpHandler, token),
		ReadHeaderTimeout: 5 * time.Second,
	}

	listenErr := make(chan error, 1)
	go func() {
		listenErr <- httpServer.ListenAndServe()
	}()

	setupLog.Info("MCP HTTP server listening on " + DefaultHTTPAddr)

	select {
	case err := <-listenErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("run MCP HTTP server: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown MCP HTTP server: %w", err)
		}
		err := <-listenErr
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("run MCP HTTP server: %w", err)
		}
		return nil
	}
}
