package controller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// enabledColumn returns the Enabled cell that kubectl get prints for obj of
// the coder.com resource, read as a server-side table from the API server.
func enabledColumn(ctx context.Context, t *testing.T, resource string, obj client.Object) any {
	t.Helper()
	server, _, err := rest.DefaultServerUrlFor(cfg)
	require.NoError(t, err)
	httpClient, err := rest.HTTPClientFor(cfg)
	require.NoError(t, err)
	server.Path = "/apis/coder.com/v1alpha1/namespaces/" + obj.GetNamespace() + "/" + resource + "/" + obj.GetName()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.String(), nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	resp, err := httpClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var table metav1.Table
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&table))
	require.Len(t, table.Rows, 1)
	for i, column := range table.ColumnDefinitions {
		if column.Name == "Enabled" {
			return table.Rows[0].Cells[i]
		}
	}
	t.Fatal("assertion failed: the table has no Enabled column")
	return nil
}

// #321: the Enabled column shows what Coder holds, not spec.enabled. A
// refused object that Coder does not have shows no value.
func TestCoderAIProviderEnabledColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("refused before a create", func(t *testing.T) {
		t.Parallel()
		env := newAIProviderEnv(ctx, t, true)
		got, _ := env.reconcile(ctx, t, env.createProvider(ctx, t, nil))
		requireAIProviderStep(t, got, true, credentialsecret.ReasonSecretNotFound)
		require.True(t, *got.Spec.Enabled)
		require.Nil(t, enabledColumn(ctx, t, "coderaiproviders", got))
	})
	t.Run("kill switch, then deleted in Coder", func(t *testing.T) {
		t.Parallel()
		env, secret, provider := newReadyAIProvider(ctx, t)
		require.Equal(t, true, enabledColumn(ctx, t, "coderaiproviders", provider))

		consented := secret.DeepCopy()
		editSecret(ctx, t, secret, func(s *corev1.Secret) { delete(s.Labels, credentialsecret.LabelKey) })
		got, _ := env.reconcile(ctx, t, provider)
		requireAIProviderStep(t, got, true, credentialsecret.ReasonNotOptedIn)
		require.Equal(t, false, enabledColumn(ctx, t, "coderaiproviders", got))

		editSecret(ctx, t, secret, func(s *corev1.Secret) { s.Labels = consented.Labels })
		require.NoError(t, env.fake.client(t, 2*time.Second).DeleteAIProvider(ctx, got.Status.ProviderID))
		got, _ = env.reconcile(ctx, t, got)
		requireAIProviderStep(t, got, true, "DeletedExternally")
		require.Nil(t, enabledColumn(ctx, t, "coderaiproviders", got))
	})
}
