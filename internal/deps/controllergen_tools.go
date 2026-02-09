//go:build tools

package deps

import (
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen" // Keep controller-gen vendored for CRD/RBAC generation.
)
