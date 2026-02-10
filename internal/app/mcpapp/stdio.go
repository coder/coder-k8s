package mcpapp

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RunStdio starts the MCP server using stdio transport.
func RunStdio(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}

	k8sClient, clientset, err := newClients()
	if err != nil {
		return err
	}

	server := NewServer(k8sClient, clientset)
	if server == nil {
		return fmt.Errorf("assertion failed: MCP server is nil after successful construction")
	}

	return server.Run(ctx, &mcp.StdioTransport{})
}
