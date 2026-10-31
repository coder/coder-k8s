package controller_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controller"
)

// TestReconcile_OperatorAccess_FailedCreateMustNotDeleteForeignToken runs the
// verifier's regression against PostgreSQL. CODER_K8S_TEST_POSTGRES_URL must
// name a disposable PostgreSQL server with password authentication and a user
// that can create databases. The test creates and drops its own database.
func TestReconcile_OperatorAccess_FailedCreateMustNotDeleteForeignToken(t *testing.T) {
	postgresURL := os.Getenv("CODER_K8S_TEST_POSTGRES_URL")
	if postgresURL == "" {
		t.Skip("set CODER_K8S_TEST_POSTGRES_URL to run the PostgreSQL regression")
	}
	ctx := context.Background()
	parsed, err := url.Parse(postgresURL)
	require.NoError(t, err)
	require.NotNil(t, parsed.User)
	password, set := parsed.User.Password()
	require.True(t, set, "PostgreSQL must require a password for this test")
	require.NotEmpty(t, password)
	admin, err := sql.Open("postgres", postgresURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	database := "operator_token_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+database)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP DATABASE "+database+" WITH (FORCE)") })
	parsed.Path = "/" + database
	good := parsed.String()
	db, err := sql.Open("postgres", good)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	schema, err := os.ReadFile("testdata/operatortoken.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(schema))
	require.NoError(t, err)
	parsed.User = url.UserPassword(parsed.User.Username(), "deliberately-wrong-password")

	ensureGatewaySchemeRegistered(t)
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: createTestNamespace(ctx, t, "operator-token-failed-create")},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:    "operator-token-test:local",
			ExtraEnv: []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: parsed.String()}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cp) })
	h := fnv.New64a()
	_, _ = h.Write([]byte(cp.Namespace))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(cp.Name))
	provisioner := coderbootstrap.NewPostgresOperatorAccessProvisioner()
	victim, err := provisioner.EnsureOperatorToken(ctx, coderbootstrap.EnsureOperatorTokenRequest{
		PostgresURL: good, OperatorUsername: "coder-k8s-operator", OperatorEmail: "coder-k8s-operator@coder-k8s.invalid",
		TokenName: fmt.Sprintf("coder-k8s-operator-%016x", h.Sum64()), TokenLifetime: time.Hour,
	})
	require.NoError(t, err)
	id, secret, ok := strings.Cut(victim, "-")
	require.True(t, ok)
	hashed := sha256.Sum256([]byte(secret))
	live := func() bool {
		var valid bool
		require.NoError(t, db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM api_keys WHERE id=$1 AND hashed_secret=$2 AND expires_at>now())", id, hashed[:]).Scan(&valid))
		return valid
	}

	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}
	result, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.False(t, getControlPlane(t, cp).Status.OperatorAccessReady)
	require.True(t, live(), "the failed Ping must not change the pre-existing token")
	replaceOperatorTokenSecret(ctx, t, cp)
	updateControlPlaneSpec(ctx, t, cp, func(spec *coderv1alpha1.CoderControlPlaneSpec) { spec.ExtraEnv[0].Value = good })
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	var deleted int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM deleted_keys").Scan(&deleted))
	require.True(t, live(), "a record before a failed create must never authorize deleting another installation's token")
	require.Zero(t, deleted)
}
