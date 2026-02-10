package main

import (
	"flag"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/coder/coder-k8s/internal/app/apiserverapp"
	"github.com/coder/coder-k8s/internal/app/controllerapp"
	"github.com/coder/coder-k8s/internal/app/mcpapp"
)

const supportedAppModes = "controller, aggregated-apiserver, mcp-stdio, mcp-http"

var (
	runControllerApp          = controllerapp.Run
	runAggregatedAPIServerApp = apiserverapp.Run
	runMCPStdioApp            = mcpapp.RunStdio
	runMCPHTTPApp             = mcpapp.RunHTTP
	setupSignalHandler        = ctrl.SetupSignalHandler
)

func run(args []string) error {
	fs := flag.NewFlagSet("coder-k8s", flag.ContinueOnError)
	var appMode string
	fs.StringVar(&appMode, "app", "", "Application mode (controller, aggregated-apiserver, mcp-stdio, mcp-http)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch appMode {
	case "controller":
		return runControllerApp(setupSignalHandler())
	case "aggregated-apiserver":
		return runAggregatedAPIServerApp(setupSignalHandler())
	case "mcp-stdio":
		return runMCPStdioApp(setupSignalHandler())
	case "mcp-http":
		return runMCPHTTPApp(setupSignalHandler())
	case "":
		return fmt.Errorf("assertion failed: --app flag is required; must be one of: %s", supportedAppModes)
	default:
		return fmt.Errorf("assertion failed: unsupported --app value %q; must be one of: %s", appMode, supportedAppModes)
	}
}
