package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/coder/coder-k8s/internal/app/apiserverapp"
	"github.com/coder/coder-k8s/internal/app/controllerapp"
	"github.com/coder/coder-k8s/internal/app/mcpapp"
)

const supportedAppModes = "controller, aggregated-apiserver, mcp-http"

var (
	runControllerApp          = controllerapp.Run
	runAggregatedAPIServerApp = func(ctx context.Context, opts apiserverapp.Options) error {
		return apiserverapp.RunWithOptions(ctx, opts)
	}
	runMCPHTTPApp      = mcpapp.RunHTTP
	setupSignalHandler = ctrl.SetupSignalHandler
)

func run(args []string) error {
	fs := flag.NewFlagSet("coder-k8s", flag.ContinueOnError)
	var (
		appMode             string
		coderURL            string
		coderSessionToken   string
		coderNamespace      string
		coderRequestTimeout time.Duration
	)
	fs.StringVar(&appMode, "app", "", "Application mode (controller, aggregated-apiserver, mcp-http)")
	fs.StringVar(
		&coderSessionToken,
		"coder-session-token",
		"",
		"Admin session token for the backing Coder deployment",
	)
	fs.StringVar(
		&coderURL,
		"coder-url",
		"",
		"Coder deployment URL (fallback when CoderControlPlane status URL is unavailable)",
	)
	fs.StringVar(
		&coderNamespace,
		"coder-namespace",
		"",
		"Restrict the aggregated API server to serve only this Kubernetes namespace",
	)
	fs.DurationVar(
		&coderRequestTimeout,
		"coder-request-timeout",
		30*time.Second,
		"Timeout for Coder SDK API requests",
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if coderURL != "" {
		parsedCoderURL, err := url.Parse(coderURL)
		if err != nil {
			return fmt.Errorf("assertion failed: invalid --coder-url %q: %w", coderURL, err)
		}
		if parsedCoderURL.Scheme == "" || parsedCoderURL.Host == "" {
			return fmt.Errorf(
				"assertion failed: invalid --coder-url %q: must include scheme and host (for example, https://coder.example.com)",
				coderURL,
			)
		}
		scheme := strings.ToLower(parsedCoderURL.Scheme)
		if scheme != "http" && scheme != "https" {
			return fmt.Errorf("assertion failed: invalid --coder-url %q: scheme must be http or https", coderURL)
		}
	}

	switch appMode {
	case "controller":
		return runControllerApp(setupSignalHandler())
	case "aggregated-apiserver":
		opts := apiserverapp.Options{
			CoderURL:            coderURL,
			CoderSessionToken:   coderSessionToken,
			CoderNamespace:      coderNamespace,
			CoderRequestTimeout: coderRequestTimeout,
		}
		return runAggregatedAPIServerApp(setupSignalHandler(), opts)
	case "mcp-http":
		return runMCPHTTPApp(setupSignalHandler())
	case "":
		return fmt.Errorf("assertion failed: --app flag is required; must be one of: %s", supportedAppModes)
	default:
		return fmt.Errorf("assertion failed: unsupported --app value %q; must be one of: %s", appMode, supportedAppModes)
	}
}
