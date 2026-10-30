package coder

import (
	"net/http"
	"net/http/httptrace"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/component-base/metrics/legacyregistry"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// connectionWait shows a saturated pool before requests time out: once a deployment has
// maxConnsPerCoderHost connections, further requests wait here for a free one.
var connectionWait = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "coder_k8s_coder_connection_wait_seconds",
	Help:    "Time a Coder API request waited for a connection to its Coder deployment, including the dial of a new connection.",
	Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30},
})

func init() {
	// The controller serves metrics.Registry and the aggregated API server serves legacyregistry.
	metrics.Registry.MustRegister(connectionWait)
	legacyregistry.Registerer().MustRegister(connectionWait)
}

// connectionWaitRoundTripper observes connectionWait for every connection a request
// waits for, also when the request ends before it gets one.
type connectionWaitRoundTripper struct {
	base http.RoundTripper
}

func (rt connectionWaitRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var (
		start   time.Time
		waiting bool
	)
	observe := func() {
		waiting = false
		connectionWait.Observe(time.Since(start).Seconds())
	}
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { start, waiting = time.Now(), true },
		GotConn: func(httptrace.GotConnInfo) { observe() },
	}

	res, err := rt.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if waiting {
		observe()
	}
	return res, err
}
