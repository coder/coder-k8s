package controller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// internalCoderURL is the internal HTTP URL of the fixture's control plane.
func internalCoderURL(f provisionerCredentialFixture) string {
	return "http://" + f.controlPlane.Name + "." + f.namespace + ".svc.cluster.local:80"
}

// TestCoderProvisioner_CoderAPIURL checks which URL the provisioner sends the
// operator token to. With an HTTPS status.url (TLS on), Coder API calls use
// the internal HTTP URL. The provisioner pods and the recorded
// status.controlPlaneURL keep status.url.
func TestCoderProvisioner_CoderAPIURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		statusURL func(f provisionerCredentialFixture) string
		wantAPI   func(f provisionerCredentialFixture) string
	}{
		{
			name:      "HTTPS status URL uses the internal HTTP URL",
			statusURL: func(provisionerCredentialFixture) string { return testCoderOrigin },
			wantAPI:   internalCoderURL,
		},
		{
			name: "HTTP status URL is kept",
			statusURL: func(f provisionerCredentialFixture) string {
				return "http://coder." + f.namespace + ".svc.cluster.local:80"
			},
			wantAPI: func(f provisionerCredentialFixture) string {
				return "http://coder." + f.namespace + ".svc.cluster.local:80"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-coderurl")
			statusURL := tt.statusURL(f)
			controlPlane := &coderv1alpha1.CoderControlPlane{}
			require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: f.controlPlane.Name, Namespace: f.namespace}, controlPlane))
			controlPlane.Status.URL = statusURL
			require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))

			provisioner := f.createProvisioner(ctx, t, nil, nil)
			f.reconcile(ctx, t, provisioner)
			f.reconcile(ctx, t, provisioner)

			wantAPI := tt.wantAPI(f)
			require.Len(t, f.bootstrapClient.entitlementsRequests, 1)
			require.Equal(t, wantAPI, f.bootstrapClient.entitlementsRequests[0].CoderURL)
			require.Len(t, f.bootstrapClient.provisionerKeyRequests, 1)
			require.Equal(t, wantAPI, f.bootstrapClient.provisionerKeyRequests[0].CoderURL)

			latest := f.get(ctx, t, provisioner)
			require.Equal(t, statusURL, latest.Status.ControlPlaneURL, "status records status.url")
			deployment, exists := f.deployment(ctx, t, provisioner)
			require.True(t, exists)
			coderURLEnv := ""
			for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
				if env.Name == "CODER_URL" {
					coderURLEnv = env.Value
				}
			}
			require.Equal(t, statusURL, coderURLEnv, "the provisioner pods keep status.url")
		})
	}
}
