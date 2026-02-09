// Package deps keeps baseline Kubernetes dependencies in go.mod for bootstrap.
//
// These imports are intentionally blank and compile-time only; they allow
// vendoring and code-generation scripts to be available in ./vendor.
package deps

import (
	_ "k8s.io/apimachinery/pkg/runtime"           // Keep apimachinery runtime dependency vendored.
	_ "k8s.io/client-go/kubernetes"               // Keep client-go kubernetes client vendored.
	_ "k8s.io/code-generator"                     // Keep code-generator scripts vendored.
	_ "sigs.k8s.io/controller-runtime/pkg/client" // Keep controller-runtime client vendored.
)
