package main

import (
	"flag"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/coder/coder-k8s/internal/app/controllerapp"
)

func run(args []string) error {
	fs := flag.NewFlagSet("coder-k8s", flag.ContinueOnError)
	var appMode string
	fs.StringVar(&appMode, "app", "", "Application mode (controller, aggregated-apiserver)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch appMode {
	case "controller":
		return controllerapp.Run(ctrl.SetupSignalHandler())
	case "aggregated-apiserver":
		return fmt.Errorf("aggregated-apiserver mode is not yet implemented")
	case "":
		return fmt.Errorf("assertion failed: --app flag is required; must be one of: controller, aggregated-apiserver")
	default:
		return fmt.Errorf("assertion failed: unsupported --app value %q; must be one of: controller, aggregated-apiserver", appMode)
	}
}
