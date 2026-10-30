package apiserverapp

import (
	"net/http"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
)

func TestMetricsServesCoderConnectionWait(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) {
		k.setDecide(func(spec authorizationv1.SubjectAccessReviewSpec) bool {
			ra := spec.NonResourceAttributes
			return ra != nil && ra.Verb == "get" && ra.Path == "/metrics"
		})
	})

	status, body := f.server.do(t, f.frontProxyCert(t), http.MethodGet, "/metrics", remoteUser("prometheus"), "")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d body=%.300s", status, body)
	}
	if !strings.Contains(body, "coder_k8s_coder_connection_wait_seconds_count") {
		t.Fatal("GET /metrics does not include coder_k8s_coder_connection_wait_seconds")
	}
}
