package mcpapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultHTTPAddr is the default listen address used by MCP HTTP mode.
	DefaultHTTPAddr = ":8090"
	// streamableHTTPSessionTimeout ensures abandoned MCP streamable HTTP sessions are reclaimed.
	streamableHTTPSessionTimeout = 15 * time.Minute
)

var setupLog = ctrl.Log.WithName("setup")

// RunHTTP starts the MCP server using streamable HTTP transport.
func RunHTTP(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}

	k8sClient, clientset, err := newClients()
	if err != nil {
		return err
	}

	return RunHTTPWithClients(ctx, k8sClient, clientset)
}

// RunHTTPWithClients starts the MCP server using streamable HTTP transport and the provided Kubernetes clients.
func RunHTTPWithClients(ctx context.Context, k8sClient client.Client, clientset kubernetes.Interface) error {
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}
	if k8sClient == nil {
		return fmt.Errorf("assertion failed: Kubernetes client must not be nil")
	}
	if clientset == nil {
		return fmt.Errorf("assertion failed: Kubernetes clientset must not be nil")
	}

	server := NewServer(k8sClient, clientset)
	if server == nil {
		return fmt.Errorf("assertion failed: MCP server is nil after successful construction")
	}

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		SessionTimeout: streamableHTTPSessionTimeout,
	})
	if mcpHandler == nil {
		return fmt.Errorf("assertion failed: MCP HTTP handler is nil after successful construction")
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	httpServer := &http.Server{
		Addr:              DefaultHTTPAddr,
		Handler:           mux,
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
