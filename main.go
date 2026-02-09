// Package main provides the entrypoint for the coder-k8s binary.
package main

import (
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	if err := run(os.Args[1:]); err != nil {
		ctrl.Log.WithName("setup").Error(err, "application failed")
		os.Exit(1)
	}
}
