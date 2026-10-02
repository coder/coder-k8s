package apiserverapp

import (
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/wsstream"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericapiserver "k8s.io/apiserver/pkg/server"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/storage"
)

// logResponseLifetime bounds how long one coderworkspaces/log response may hold its
// connection: storage stops reading from Coder after MaxWorkspaceLogDuration, and the outer
// guard's write deadline ends a response whose client stopped reading one minute later.
const logResponseLifetime = storage.MaxWorkspaceLogDuration + time.Minute

// newLogGuardedHandlerChain wraps the generic handler chain with the two log guards.
//
// The generic timeout filter cannot abort a write that blocks on a stalled client, and the
// serving stack sets no write timeout. outerLogGuard therefore bounds log responses with a write
// deadline. It wraps the whole chain so it holds the raw net/http writer and does not depend on
// every generic writer decorator supporting http.ResponseController. innerLogGuard runs
// after authentication and authorization and refuses upgrades on the log path, because the
// websocket path of StreamObject hijacks the connection with no deadline.
func newLogGuardedHandlerChain(lifetime time.Duration) func(http.Handler, *genericapiserver.Config) http.Handler {
	if lifetime <= 0 {
		panic("assertion failed: log response lifetime must be positive")
	}
	return func(apiHandler http.Handler, c *genericapiserver.Config) http.Handler {
		if apiHandler == nil || c == nil {
			panic("assertion failed: API handler and server config must not be nil")
		}
		if c.RequestInfoResolver == nil || c.Serializer == nil {
			panic("assertion failed: completed server config must have a request info resolver and serializer")
		}
		inner := innerLogGuard(apiHandler, c.Serializer)
		return outerLogGuard(genericapiserver.DefaultBuildHandlerChain(inner, c), c.RequestInfoResolver, lifetime)
	}
}

// validateLogResponseLifetime fails startup when a log response could outlive the request
// deadline, which would let the generic timeout filter answer while the handler still writes.
func validateLogResponseLifetime(lifetime, requestTimeout time.Duration) error {
	if lifetime <= 0 || lifetime >= requestTimeout {
		return fmt.Errorf("assertion failed: log response lifetime %s must be positive and below the request timeout %s", lifetime, requestTimeout)
	}
	return nil
}

// isWorkspaceLogRequest reports whether info names the coderworkspaces/log subresource.
func isWorkspaceLogRequest(info *apirequest.RequestInfo) bool {
	return info != nil &&
		info.IsResourceRequest &&
		info.APIGroup == aggregationv1alpha1.SchemeGroupVersion.Group &&
		info.Resource == "coderworkspaces" &&
		info.Subresource == "log"
}

// outerLogGuard sets a write deadline on the raw connection for coderworkspaces/log requests.
// It changes no status code on any path, and authentication and authorization still run inside
// it. On HTTP/2 the deadline applies to the log stream only, not to other streams on the
// connection. On HTTP/1 net/http clears it after the response is finished, so a kept-alive
// connection's next request starts without it. The guard must not clear it itself: the final
// flush after the handler returns would then run without a deadline.
func outerLogGuard(next http.Handler, resolver apirequest.RequestInfoResolver, lifetime time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, err := resolver.NewRequestInfo(r)
		if err != nil || !isWorkspaceLogRequest(info) {
			// The generic chain resolves the request again and answers resolution errors itself.
			next.ServeHTTP(w, r)
			return
		}
		controller := http.NewResponseController(w)
		if err := controller.SetWriteDeadline(time.Now().Add(lifetime)); err != nil {
			// Fail closed: a log response without a write deadline could hold the connection forever.
			http.Error(w, "assertion failed: log response writer does not support write deadlines", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// innerLogGuard refuses websocket and other upgrade requests on coderworkspaces/log.
func innerLogGuard(next http.Handler, serializer runtime.NegotiatedSerializer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, ok := apirequest.RequestInfoFrom(r.Context())
		if ok && isWorkspaceLogRequest(info) && (wsstream.IsWebSocketRequest(r) || httpstream.IsUpgradeRequest(r)) {
			err := apierrors.NewBadRequest("coderworkspaces/log does not support websocket or other upgrade requests; use a plain GET")
			responsewriters.ErrorNegotiated(err, serializer, aggregationv1alpha1.SchemeGroupVersion, w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
