package coder

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/coder/coder/v2/codersdk"
	"github.com/go-logr/logr/funcr"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

// TestMappedErrorsLogTheFullError: the messages for a Coder 403, a Coder 500, an unreachable Coder
// and an uncertain write leave out the Coder URL, and the coder-errors log keeps the full error.
// It replaces errorLog, so it does not run in parallel.
func TestMappedErrorsLogTheFullError(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	previous := errorLog
	errorLog = funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{Verbosity: 1})
	t.Cleanup(func() { errorLog = previous })

	resource := aggregationv1alpha1.Resource("coderworkspaces")
	const coderURL = "https://coder.internal.example.com/api/v2/workspaces/1234"
	withMessage := func(status int) error {
		coderErr := codersdk.NewTestError(status, http.MethodPost, coderURL)
		coderErr.Message = "Coder says no."
		return coderErr
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"Coder 403", MapCoderError(withMessage(http.StatusForbidden), resource, "acme.alice.a")},
		{"Coder 500", MapCoderError(withMessage(http.StatusInternalServerError), resource, "acme.alice.b")},
		{"unreachable", MapCoderError(&url.Error{Op: "Get", URL: coderURL, Err: errors.New("connection refused")}, resource, "acme.alice.c")},
		{"uncertain write", MapCoderWriteError(withMessage(http.StatusBadGateway), WriteCreate, resource, "acme.alice.d")},
	} {
		if strings.Contains(tc.err.Error(), "coder.internal.example.com") {
			t.Fatalf("%s: the message %q names the Coder URL", tc.name, tc.err.Error())
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"acme.alice.a", "acme.alice.b", "acme.alice.c", "acme.alice.d"} {
		found := false
		for _, line := range lines {
			found = found || (strings.Contains(line, fmt.Sprintf("%q", name)) && strings.Contains(line, "coder.internal.example.com"))
		}
		if !found {
			t.Fatalf("the coder-errors log has no line with the full error for %s: %q", name, lines)
		}
	}
}
