package mcpapp

import (
	"testing"

	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestNewServerDoesNotPanic(t *testing.T) {
	t.Helper()

	k8sClient := mustNewFakeClient(t)
	clientset := k8sfake.NewClientset()
	if clientset == nil {
		t.Fatal("expected non-nil clientset")
	}

	defer func() {
		recovered := recover()
		if recovered != nil {
			t.Fatalf("expected NewServer not to panic, got %v", recovered)
		}
	}()

	server := NewServer(k8sClient, clientset)
	if server == nil {
		t.Fatal("expected non-nil MCP server")
	}
}
