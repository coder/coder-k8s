// Package sharedscheme provides reusable runtime scheme construction across app modes.
package sharedscheme

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// New builds a runtime scheme with core Kubernetes, coder.com, and aggregation.coder.com APIs.
func New() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(coderv1alpha1.AddToScheme(scheme))
	utilruntime.Must(aggregationv1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	return scheme
}
