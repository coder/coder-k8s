// Package deps keeps baseline Kubernetes dependencies in go.mod for bootstrap.
//
// These imports are intentionally blank and compile-time only; they allow
// vendoring and code-generation scripts to be available in ./vendor.
package deps

import (
	_ "k8s.io/apimachinery/pkg/runtime"
	_ "k8s.io/client-go/kubernetes"
	_ "k8s.io/code-generator"
	_ "sigs.k8s.io/controller-runtime/pkg/client"
)
