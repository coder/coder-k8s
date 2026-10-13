package controller_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controller"
	"github.com/coder/coder-k8s/internal/credentialsecret"
	"github.com/coder/coder-k8s/internal/credentialsecret/credentialsecrettest"
)

const (
	testCoderOrigin     = "https://coder.example.com"
	testProxyTokenValue = "proxy-token-9c1d7e5a" // #nosec G101 -- test fixture, not a credential.
	testBootstrapValue  = "bootstrap-session-4b2a8f6c"
	testGeneratedToken  = "generated-proxy-token-71e3"
	testOperatorToken   = "operator-session-token"
	testExtraEnvValue   = "extra-env-credential-d04f"
)

// consentedObjectMeta returns Secret metadata that opts the Secret in to the
// credential Secret contract for origins.
func consentedObjectMeta(name, namespace string, origins ...string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   namespace,
		Labels:      map[string]string{credentialsecret.LabelKey: credentialsecret.LabelValue},
		Annotations: map[string]string{credentialsecret.DestinationsAnnotation: strings.Join(origins, ",")},
	}
}

func createSecret(ctx context.Context, t *testing.T, secret *corev1.Secret) *corev1.Secret {
	t.Helper()
	require.NoError(t, k8sClient.Create(ctx, secret))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), secret) })
	return secret
}

func tokenSecret(meta metav1.ObjectMeta, key, value string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: meta, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{key: []byte(value)}}
}

// logCapture collects every log line a reconcile writes.
type logCapture struct {
	mu    sync.Mutex
	lines strings.Builder
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lines.String()
}

func capturingContext(ctx context.Context) (context.Context, *logCapture) {
	capture := &logCapture{}
	logger := funcr.New(func(prefix, args string) {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		capture.lines.WriteString(prefix + " " + args + "\n")
	}, funcr.Options{Verbosity: 10})
	return logr.NewContext(ctx, logger), capture
}

func createWorkspaceProxy(ctx context.Context, t *testing.T, namespace, name string, spec coderv1alpha1.WorkspaceProxySpec) *coderv1alpha1.CoderWorkspaceProxy {
	t.Helper()
	workspaceProxy := &coderv1alpha1.CoderWorkspaceProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
	require.NoError(t, k8sClient.Create(ctx, workspaceProxy))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workspaceProxy) })
	return workspaceProxy
}

func reconcileWorkspaceProxy(ctx context.Context, t *testing.T, r *controller.CoderWorkspaceProxyReconciler, workspaceProxy *coderv1alpha1.CoderWorkspaceProxy) error {
	t.Helper()
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}})
	require.Equal(t, ctrl.Result{}, result)
	return err
}

func getWorkspaceProxy(ctx context.Context, t *testing.T, workspaceProxy *coderv1alpha1.CoderWorkspaceProxy) *coderv1alpha1.CoderWorkspaceProxy {
	t.Helper()
	latest := &coderv1alpha1.CoderWorkspaceProxy{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}, latest))
	return latest
}

func getProxyDeployment(ctx context.Context, t *testing.T, workspaceProxy *coderv1alpha1.CoderWorkspaceProxy) (*appsv1.Deployment, bool) {
	t.Helper()
	deployment := &appsv1.Deployment{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: workspaceProxyResourceName(workspaceProxy.Name), Namespace: workspaceProxy.Namespace}, deployment)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return deployment, true
}

func requireCredentialsReady(t *testing.T, conditions []metav1.Condition, status metav1.ConditionStatus, reason string) metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(conditions, "CredentialsReady")
	require.NotNil(t, condition, "expected a CredentialsReady condition")
	require.Equal(t, status, condition.Status, "CredentialsReady message: %s", condition.Message)
	require.Equal(t, reason, condition.Reason, "CredentialsReady message: %s", condition.Message)
	return *condition
}

// requireProxyFailedStatic checks the fail-static outcome of one reconcile: no
// error, CredentialsReady=False with reason, no bootstrap call, no Deployment,
// and no secret value in status, logs or the error.
func requireProxyFailedStatic(
	ctx context.Context,
	t *testing.T,
	r *controller.CoderWorkspaceProxyReconciler,
	bootstrapClient *fakeBootstrapClient,
	workspaceProxy *coderv1alpha1.CoderWorkspaceProxy,
	reason string,
) metav1.Condition {
	t.Helper()
	logCtx, logs := capturingContext(ctx)
	err := reconcileWorkspaceProxy(logCtx, t, r, workspaceProxy)
	require.NoError(t, err)

	latest := getWorkspaceProxy(ctx, t, workspaceProxy)
	condition := requireCredentialsReady(t, latest.Status.Conditions, metav1.ConditionFalse, reason)
	if bootstrapClient != nil {
		require.Equal(t, 0, bootstrapClient.calls, "expected no bootstrap request")
	}
	_, exists := getProxyDeployment(ctx, t, workspaceProxy)
	require.False(t, exists, "expected no Deployment after a failed check")
	credentialsecrettest.RequireNoLeak(t,
		[]string{testProxyTokenValue, testBootstrapValue, testOperatorToken, testExtraEnvValue, testGeneratedToken},
		map[string]any{"status": latest.Status, "annotations": latest.Annotations, "logs": logs.String()},
	)
	return condition
}

func TestCoderWorkspaceProxyCredentials_ProxySessionTokenSecretChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		secret     func(namespace string) *corev1.Secret
		ref        coderv1alpha1.SecretKeySelector
		wantReason string
	}{
		{
			name: "unlabeled secret",
			secret: func(namespace string) *corev1.Secret {
				return tokenSecret(metav1.ObjectMeta{Name: "proxy-token", Namespace: namespace}, "token", testProxyTokenValue)
			},
			ref:        coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
			wantReason: credentialsecret.ReasonNotOptedIn,
		},
		{
			name: "label without destinations annotation",
			secret: func(namespace string) *corev1.Secret {
				meta := consentedObjectMeta("proxy-token", namespace, testCoderOrigin)
				meta.Annotations = nil
				return tokenSecret(meta, "token", testProxyTokenValue)
			},
			ref:        coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
			wantReason: credentialsecret.ReasonNotOptedIn,
		},
		{
			name: "destination not allowed",
			secret: func(namespace string) *corev1.Secret {
				return tokenSecret(consentedObjectMeta("proxy-token", namespace, "https://other.example.com"), "token", testProxyTokenValue)
			},
			ref:        coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
			wantReason: credentialsecret.ReasonDestinationNotAllowed,
		},
		{
			name:       "missing secret",
			ref:        coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
			wantReason: credentialsecret.ReasonSecretNotFound,
		},
		{
			name: "missing key",
			secret: func(namespace string) *corev1.Secret {
				return tokenSecret(consentedObjectMeta("proxy-token", namespace, testCoderOrigin), "other", testProxyTokenValue)
			},
			ref:        coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
			wantReason: credentialsecret.ReasonKeyMissing,
		},
		{
			name: "value with trailing newline",
			secret: func(namespace string) *corev1.Secret {
				return tokenSecret(consentedObjectMeta("proxy-token", namespace, testCoderOrigin), "token", testProxyTokenValue+"\n")
			},
			ref:        coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
			wantReason: credentialsecret.ReasonMalformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			namespace := createTestNamespace(ctx, t, "cwp-direct")
			if tt.secret != nil {
				createSecret(ctx, t, tt.secret(namespace))
			}
			ref := tt.ref
			workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", coderv1alpha1.WorkspaceProxySpec{
				PrimaryAccessURL:           testCoderOrigin,
				ProxySessionTokenSecretRef: &ref,
			})

			r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme}
			condition := requireProxyFailedStatic(ctx, t, r, nil, workspaceProxy, tt.wantReason)
			require.Contains(t, condition.Message, `spec.proxySessionTokenSecretRef: Secret "proxy-token"`)
			require.Contains(t, condition.Message, "The proxy keeps its last applied configuration.")
		})
	}
}

func TestCoderWorkspaceProxyCredentials_RefusesOperatorTokenSecret(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"bootstrap.credentialsSecretRef", "proxySessionTokenSecretRef", "extraEnv"} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			namespace := createTestNamespace(ctx, t, "cwp-operator")
			controlPlane := createTestControlPlane(ctx, t, namespace, "coder", testCoderOrigin)

			// Opting the operator token in must not make it usable.
			operatorSecret := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: controlPlane.Status.OperatorTokenSecretRef.Name, Namespace: namespace}, operatorSecret))
			consented := consentedObjectMeta(operatorSecret.Name, namespace, testCoderOrigin)
			operatorSecret.Labels = consented.Labels
			operatorSecret.Annotations = consented.Annotations
			require.NoError(t, k8sClient.Update(ctx, operatorSecret))

			operatorRef := coderv1alpha1.SecretKeySelector{Name: operatorSecret.Name, Key: "token"}
			spec := coderv1alpha1.WorkspaceProxySpec{PrimaryAccessURL: testCoderOrigin}
			switch input {
			case "bootstrap.credentialsSecretRef":
				spec.Bootstrap = &coderv1alpha1.ProxyBootstrapSpec{CoderURL: testCoderOrigin, CredentialsSecretRef: operatorRef}
			case "proxySessionTokenSecretRef":
				spec.ProxySessionTokenSecretRef = &operatorRef
			case "extraEnv":
				createSecret(ctx, t, tokenSecret(consentedObjectMeta("proxy-token", namespace, testCoderOrigin), "token", testProxyTokenValue))
				spec.ProxySessionTokenSecretRef = &coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"}
				spec.ExtraEnv = []corev1.EnvVar{{Name: "SOME_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: operatorSecret.Name}, Key: "token",
				}}}}
			}
			workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", spec)

			bootstrapClient := &fakeBootstrapClient{response: coderbootstrap.RegisterWorkspaceProxyResponse{ProxyToken: testGeneratedToken}}
			r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
			condition := requireProxyFailedStatic(ctx, t, r, bootstrapClient, workspaceProxy, credentialsecret.ReasonRefused)
			require.Contains(t, condition.Message, fmt.Sprintf("Secret %q is managed by coder-k8s", operatorSecret.Name))
		})
	}
}

func TestCoderWorkspaceProxyCredentials_BootstrapCredentialsChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		meta             func(namespace string) metav1.ObjectMeta
		primaryAccessURL string
		wantReason       string
		wantMessage      string
	}{
		{
			name: "unlabeled bootstrap credentials",
			meta: func(namespace string) metav1.ObjectMeta {
				return metav1.ObjectMeta{Name: "bootstrap", Namespace: namespace}
			},
			wantReason: credentialsecret.ReasonNotOptedIn,
		},
		{
			name: "coderURL not in destinations",
			meta: func(namespace string) metav1.ObjectMeta {
				return consentedObjectMeta("bootstrap", namespace, "https://other.example.com")
			},
			wantReason:  credentialsecret.ReasonDestinationNotAllowed,
			wantMessage: "does not allow https://coder.example.com",
		},
		{
			name: "primary access URL not in destinations",
			meta: func(namespace string) metav1.ObjectMeta {
				return consentedObjectMeta("bootstrap", namespace, testCoderOrigin)
			},
			primaryAccessURL: "https://proxy-target.example.net",
			wantReason:       credentialsecret.ReasonDestinationNotAllowed,
			wantMessage:      "(proxy token destination): Secret \"bootstrap\" does not allow https://proxy-target.example.net",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			namespace := createTestNamespace(ctx, t, "cwp-bootstrap")
			createSecret(ctx, t, tokenSecret(tt.meta(namespace), "token", testBootstrapValue))
			workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", coderv1alpha1.WorkspaceProxySpec{
				PrimaryAccessURL: tt.primaryAccessURL,
				Bootstrap: &coderv1alpha1.ProxyBootstrapSpec{
					CoderURL:             testCoderOrigin,
					CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: "bootstrap", Key: "token"},
				},
			})

			bootstrapClient := &fakeBootstrapClient{response: coderbootstrap.RegisterWorkspaceProxyResponse{ProxyToken: testGeneratedToken}}
			r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
			condition := requireProxyFailedStatic(ctx, t, r, bootstrapClient, workspaceProxy, tt.wantReason)
			require.Contains(t, condition.Message, "spec.bootstrap.credentialsSecretRef")
			if tt.wantMessage != "" {
				require.Contains(t, condition.Message, tt.wantMessage)
			}

			// The generated token Secret was never created.
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "proxy-proxy-token", Namespace: namespace}, &corev1.Secret{})
			require.True(t, apierrors.IsNotFound(err), "expected no generated token Secret, got %v", err)
		})
	}
}

func TestCoderWorkspaceProxyCredentials_ExtraEnvSecretChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		meta       func(namespace string) metav1.ObjectMeta
		wantReason string
	}{
		{
			name: "unlabeled extraEnv secret",
			meta: func(namespace string) metav1.ObjectMeta {
				return metav1.ObjectMeta{Name: "extra", Namespace: namespace}
			},
			wantReason: credentialsecret.ReasonNotOptedIn,
		},
		{
			name: "extraEnv secret for another destination",
			meta: func(namespace string) metav1.ObjectMeta {
				return consentedObjectMeta("extra", namespace, "https://other.example.com")
			},
			wantReason: credentialsecret.ReasonDestinationNotAllowed,
		},
		{
			name: "consented extraEnv secret",
			meta: func(namespace string) metav1.ObjectMeta {
				return consentedObjectMeta("extra", namespace, testCoderOrigin)
			},
			wantReason: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			namespace := createTestNamespace(ctx, t, "cwp-extraenv")
			createSecret(ctx, t, tokenSecret(consentedObjectMeta("proxy-token", namespace, testCoderOrigin), "token", testProxyTokenValue))
			createSecret(ctx, t, tokenSecret(tt.meta(namespace), "value", testExtraEnvValue))
			workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", coderv1alpha1.WorkspaceProxySpec{
				PrimaryAccessURL:           testCoderOrigin,
				ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
				ExtraEnv: []corev1.EnvVar{
					{Name: "PLAIN", Value: "plain"},
					{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "extra"}, Key: "value",
					}}},
				},
			})

			r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme}
			if tt.wantReason != "" {
				condition := requireProxyFailedStatic(ctx, t, r, nil, workspaceProxy, tt.wantReason)
				require.Contains(t, condition.Message, `spec.extraEnv[1] (FROM_SECRET): Secret "extra"`)
				return
			}

			require.NoError(t, reconcileWorkspaceProxy(ctx, t, r, workspaceProxy))
			requireCredentialsReady(t, getWorkspaceProxy(ctx, t, workspaceProxy).Status.Conditions, metav1.ConditionTrue, "CredentialsAccepted")
			_, exists := getProxyDeployment(ctx, t, workspaceProxy)
			require.True(t, exists)
		})
	}
}

func TestCoderWorkspaceProxyCredentials_ReservedSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		env      []corev1.EnvVar
		args     []string
		reserved string
	}{
		{name: "env CODER_PRIMARY_ACCESS_URL", env: []corev1.EnvVar{{Name: "CODER_PRIMARY_ACCESS_URL", Value: "https://other.example.com"}}, reserved: "spec.extraEnv sets CODER_PRIMARY_ACCESS_URL"},
		{name: "env CODER_PROXY_SESSION_TOKEN", env: []corev1.EnvVar{{Name: "CODER_PROXY_SESSION_TOKEN", Value: "x"}}, reserved: "spec.extraEnv sets CODER_PROXY_SESSION_TOKEN"},
		{name: "flag --primary-access-url value", args: []string{"--primary-access-url", "https://other.example.com"}, reserved: "spec.extraArgs passes --primary-access-url"},
		{name: "flag --primary-access-url=value", args: []string{"--primary-access-url=https://other.example.com"}, reserved: "spec.extraArgs passes --primary-access-url"},
		{name: "flag -primary-access-url value", args: []string{"-primary-access-url", "https://other.example.com"}, reserved: "spec.extraArgs passes --primary-access-url"},
		{name: "flag --proxy-session-token value", args: []string{"--proxy-session-token", "x"}, reserved: "spec.extraArgs passes --proxy-session-token"},
		{name: "flag --proxy-session-token=value", args: []string{"--proxy-session-token=x"}, reserved: "spec.extraArgs passes --proxy-session-token"},
		{name: "flag -proxy-session-token=value", args: []string{"-proxy-session-token=x"}, reserved: "spec.extraArgs passes --proxy-session-token"},
		{name: "env lowercase coder_primary_access_url", env: []corev1.EnvVar{{Name: "coder_primary_access_url", Value: "https://other.example.com"}}, reserved: "spec.extraEnv sets coder_primary_access_url"},
		{
			name:     "flag hidden in an env reference",
			env:      []corev1.EnvVar{{Name: "X", Value: "--primary-access-url=https://other.example.com"}},
			args:     []string{"$(X)"},
			reserved: "spec.extraArgs passes --primary-access-url",
		},
		{name: "look-alike env and flags pass", env: []corev1.EnvVar{{Name: "CODER_PRIMARY_ACCESS_URL_EXTRA", Value: "x"}}, args: []string{"--primary-access-urls=x", "--proxy-session-tokenless", "primary-access-url"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			namespace := createTestNamespace(ctx, t, "cwp-reserved")
			createSecret(ctx, t, tokenSecret(consentedObjectMeta("proxy-token", namespace, testCoderOrigin), "token", testProxyTokenValue))
			workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", coderv1alpha1.WorkspaceProxySpec{
				PrimaryAccessURL:           testCoderOrigin,
				ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
				ExtraEnv:                   tt.env,
				ExtraArgs:                  tt.args,
			})

			r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme}
			if tt.reserved == "" {
				require.NoError(t, reconcileWorkspaceProxy(ctx, t, r, workspaceProxy))
				requireCredentialsReady(t, getWorkspaceProxy(ctx, t, workspaceProxy).Status.Conditions, metav1.ConditionTrue, "CredentialsAccepted")
				_, exists := getProxyDeployment(ctx, t, workspaceProxy)
				require.True(t, exists)
				return
			}
			condition := requireProxyFailedStatic(ctx, t, r, nil, workspaceProxy, "ReservedSettingOverride")
			require.Contains(t, condition.Message, tt.reserved)
			require.NotContains(t, condition.Message, "https://other.example.com", "the message must not echo the value")
		})
	}
}

func TestCoderWorkspaceProxyCredentials_TokenSecretNotOwned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup creates the conflicting Secret and returns the
		// generatedTokenSecretName to use ("" for the default name).
		setup func(ctx context.Context, t *testing.T, namespace string) (string, *corev1.Secret)
	}{
		{
			name: "named Secret without owner",
			setup: func(ctx context.Context, t *testing.T, namespace string) (string, *corev1.Secret) {
				return "existing-token", createSecret(ctx, t, tokenSecret(metav1.ObjectMeta{Name: "existing-token", Namespace: namespace}, "token", testProxyTokenValue))
			},
		},
		{
			name: "default name Secret without owner",
			setup: func(ctx context.Context, t *testing.T, namespace string) (string, *corev1.Secret) {
				return "", createSecret(ctx, t, tokenSecret(metav1.ObjectMeta{Name: "proxy-proxy-token", Namespace: namespace}, "token", testProxyTokenValue))
			},
		},
		{
			name: "token Secret of another proxy",
			setup: func(ctx context.Context, t *testing.T, namespace string) (string, *corev1.Secret) {
				other := createWorkspaceProxy(ctx, t, namespace, "other", coderv1alpha1.WorkspaceProxySpec{PrimaryAccessURL: testCoderOrigin})
				secret := tokenSecret(metav1.ObjectMeta{Name: "other-proxy-token", Namespace: namespace}, "token", testProxyTokenValue)
				require.NoError(t, controllerutil.SetControllerReference(other, secret, scheme))
				return "other-proxy-token", createSecret(ctx, t, secret)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			namespace := createTestNamespace(ctx, t, "cwp-notowned")
			createSecret(ctx, t, tokenSecret(consentedObjectMeta("bootstrap", namespace, testCoderOrigin), "token", testBootstrapValue))
			generatedName, conflicting := tt.setup(ctx, t, namespace)
			before := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(conflicting), before))

			workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", coderv1alpha1.WorkspaceProxySpec{
				Bootstrap: &coderv1alpha1.ProxyBootstrapSpec{
					CoderURL:                 testCoderOrigin,
					CredentialsSecretRef:     coderv1alpha1.SecretKeySelector{Name: "bootstrap", Key: "token"},
					GeneratedTokenSecretName: generatedName,
				},
			})

			bootstrapClient := &fakeBootstrapClient{response: coderbootstrap.RegisterWorkspaceProxyResponse{ProxyToken: testGeneratedToken}}
			r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
			condition := requireProxyFailedStatic(ctx, t, r, bootstrapClient, workspaceProxy, "TokenSecretNotOwned")
			require.Contains(t, condition.Message, fmt.Sprintf("Secret %q", conflicting.Name))

			after := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(conflicting), after))
			require.Equal(t, before.ResourceVersion, after.ResourceVersion, "the conflicting Secret must not change")
			require.Equal(t, before.Data, after.Data)
			require.Equal(t, before.OwnerReferences, after.OwnerReferences)
			require.Equal(t, before.Labels, after.Labels)
		})
	}
}

func TestCoderWorkspaceProxyCredentials_OwnedTokenSecretIsReusedAndUpdated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "cwp-owned")
	createSecret(ctx, t, tokenSecret(consentedObjectMeta("bootstrap", namespace, testCoderOrigin), "token", testBootstrapValue))
	workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", coderv1alpha1.WorkspaceProxySpec{
		Bootstrap: &coderv1alpha1.ProxyBootstrapSpec{
			CoderURL:             testCoderOrigin,
			CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: "bootstrap", Key: "token"},
		},
	})

	bootstrapClient := &fakeBootstrapClient{response: coderbootstrap.RegisterWorkspaceProxyResponse{ProxyToken: testGeneratedToken}}
	r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}

	// First reconcile: the Secret is absent, so the controller creates it.
	require.NoError(t, reconcileWorkspaceProxy(ctx, t, r, workspaceProxy))
	require.Equal(t, 1, bootstrapClient.calls)
	latest := getWorkspaceProxy(ctx, t, workspaceProxy)
	requireCredentialsReady(t, latest.Status.Conditions, metav1.ConditionTrue, "CredentialsAccepted")
	require.True(t, latest.Status.Registered)

	key := types.NamespacedName{Name: "proxy-proxy-token", Namespace: namespace}
	generated := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, key, generated))
	require.Equal(t, testGeneratedToken, string(generated.Data["token"]))
	owner := metav1.GetControllerOf(generated)
	require.NotNil(t, owner)
	require.Equal(t, latest.UID, owner.UID)

	deployment, exists := getProxyDeployment(ctx, t, workspaceProxy)
	require.True(t, exists)
	var tokenEnv *corev1.EnvVar
	for i := range deployment.Spec.Template.Spec.Containers[0].Env {
		if env := &deployment.Spec.Template.Spec.Containers[0].Env[i]; env.Name == "CODER_PROXY_SESSION_TOKEN" {
			tokenEnv = env
		}
	}
	require.NotNil(t, tokenEnv)
	require.Equal(t, key.Name, tokenEnv.ValueFrom.SecretKeyRef.Name)

	// Second reconcile: the owned Secret holds a token, so it is mounted as is.
	require.NoError(t, reconcileWorkspaceProxy(ctx, t, r, workspaceProxy))
	require.Equal(t, 1, bootstrapClient.calls)

	// An owned Secret without a token is updated in place, and keeps its owner.
	require.NoError(t, k8sClient.Get(ctx, key, generated))
	generated.Data = map[string][]byte{"token": nil}
	require.NoError(t, k8sClient.Update(ctx, generated))
	bootstrapClient.response.ProxyToken = testGeneratedToken + "-rotated"
	require.NoError(t, reconcileWorkspaceProxy(ctx, t, r, workspaceProxy))
	require.Equal(t, 2, bootstrapClient.calls)
	updated := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, key, updated))
	require.Equal(t, testGeneratedToken+"-rotated", string(updated.Data["token"]))
	require.Equal(t, generated.OwnerReferences, updated.OwnerReferences)
}

func TestCoderWorkspaceProxyCredentials_FailedCheckLeavesDeploymentUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "cwp-static")
	tokenSecretObj := createSecret(ctx, t, tokenSecret(consentedObjectMeta("proxy-token", namespace, testCoderOrigin), "token", testProxyTokenValue))
	workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", coderv1alpha1.WorkspaceProxySpec{
		Image:                      "proxy:v1",
		PrimaryAccessURL:           testCoderOrigin,
		ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
	})
	r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme}

	// The consented path works as before.
	require.NoError(t, reconcileWorkspaceProxy(ctx, t, r, workspaceProxy))
	before, exists := getProxyDeployment(ctx, t, workspaceProxy)
	require.True(t, exists)
	require.Equal(t, "proxy:v1", before.Spec.Template.Spec.Containers[0].Image)

	// Remove consent and change the spec: the Deployment must not change.
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(tokenSecretObj), tokenSecretObj))
	delete(tokenSecretObj.Labels, credentialsecret.LabelKey)
	require.NoError(t, k8sClient.Update(ctx, tokenSecretObj))
	latest := getWorkspaceProxy(ctx, t, workspaceProxy)
	latest.Spec.Image = "proxy:v2"
	latest.Spec.ExtraEnv = []corev1.EnvVar{{Name: "CODER_PRIMARY_ACCESS_URL", Value: "https://other.example.com"}}
	require.NoError(t, k8sClient.Update(ctx, latest))

	logCtx, logs := capturingContext(ctx)
	require.NoError(t, reconcileWorkspaceProxy(logCtx, t, r, workspaceProxy))
	requireCredentialsReady(t, getWorkspaceProxy(ctx, t, workspaceProxy).Status.Conditions, metav1.ConditionFalse, "ReservedSettingOverride")
	after, exists := getProxyDeployment(ctx, t, workspaceProxy)
	require.True(t, exists)
	require.Equal(t, before.ResourceVersion, after.ResourceVersion, "the Deployment must stay byte-identical")
	require.Equal(t, before.Spec, after.Spec)
	require.Contains(t, logs.String(), "ReservedSettingOverride")

	// Fix the reserved override: the missing label is the next failure.
	latest = getWorkspaceProxy(ctx, t, workspaceProxy)
	latest.Spec.ExtraEnv = nil
	require.NoError(t, k8sClient.Update(ctx, latest))
	require.NoError(t, reconcileWorkspaceProxy(ctx, t, r, workspaceProxy))
	requireCredentialsReady(t, getWorkspaceProxy(ctx, t, workspaceProxy).Status.Conditions, metav1.ConditionFalse, credentialsecret.ReasonNotOptedIn)
	after, _ = getProxyDeployment(ctx, t, workspaceProxy)
	require.Equal(t, before.ResourceVersion, after.ResourceVersion)

	// Restore consent: the pending spec change is applied.
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(tokenSecretObj), tokenSecretObj))
	tokenSecretObj.Labels = map[string]string{credentialsecret.LabelKey: credentialsecret.LabelValue}
	require.NoError(t, k8sClient.Update(ctx, tokenSecretObj))
	require.NoError(t, reconcileWorkspaceProxy(ctx, t, r, workspaceProxy))
	requireCredentialsReady(t, getWorkspaceProxy(ctx, t, workspaceProxy).Status.Conditions, metav1.ConditionTrue, "CredentialsAccepted")
	after, _ = getProxyDeployment(ctx, t, workspaceProxy)
	require.Equal(t, "proxy:v2", after.Spec.Template.Spec.Containers[0].Image)
}

func TestCoderWorkspaceProxyReconcile_DisablesServiceAccountTokenAutomount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "cwp-automount")
	createSecret(ctx, t, tokenSecret(consentedObjectMeta("proxy-token", namespace, testCoderOrigin), "token", testProxyTokenValue))
	workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", coderv1alpha1.WorkspaceProxySpec{
		PrimaryAccessURL:           testCoderOrigin,
		ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
	})
	r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme}
	require.NoError(t, reconcileWorkspaceProxy(ctx, t, r, workspaceProxy))

	deployment, exists := getProxyDeployment(ctx, t, workspaceProxy)
	require.True(t, exists)
	automount := deployment.Spec.Template.Spec.AutomountServiceAccountToken
	require.NotNil(t, automount)
	require.False(t, *automount)
}

func TestCoderWorkspaceProxyCredentials_BootstrapErrorIsRedacted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "cwp-redact")
	bootstrapSecret := createSecret(ctx, t, tokenSecret(metav1.ObjectMeta{Name: "bootstrap", Namespace: namespace}, "token", testBootstrapValue))
	workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", coderv1alpha1.WorkspaceProxySpec{
		Bootstrap: &coderv1alpha1.ProxyBootstrapSpec{
			CoderURL:             testCoderOrigin,
			CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: "bootstrap", Key: "token"},
		},
	})
	bootstrapClient := &fakeBootstrapClient{err: errors.New("request with Coder-Session-Token " + testBootstrapValue + " failed")}
	r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
	requireProxyFailedStatic(ctx, t, r, bootstrapClient, workspaceProxy, credentialsecret.ReasonNotOptedIn)

	// Opt the Secret in. The checks pass, and the Coder request fails.
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(bootstrapSecret), bootstrapSecret))
	consented := consentedObjectMeta("bootstrap", namespace, testCoderOrigin)
	bootstrapSecret.Labels, bootstrapSecret.Annotations = consented.Labels, consented.Annotations
	require.NoError(t, k8sClient.Update(ctx, bootstrapSecret))

	logCtx, logs := capturingContext(ctx)
	err := reconcileWorkspaceProxy(logCtx, t, r, workspaceProxy)
	require.Error(t, err)
	require.Contains(t, err.Error(), "[REDACTED]")
	require.Equal(t, 1, bootstrapClient.calls)
	// The condition reports the passed checks, not the earlier failure.
	requireCredentialsReady(t, getWorkspaceProxy(ctx, t, workspaceProxy).Status.Conditions, metav1.ConditionTrue, "CredentialsAccepted")
	credentialsecrettest.RequireNoLeak(t, []string{testBootstrapValue}, map[string]any{
		"error":  err,
		"logs":   logs.String(),
		"status": getWorkspaceProxy(ctx, t, workspaceProxy).Status,
	})
}

// TestCoderWorkspaceProxyCredentials_SecretWatchRequeues runs the controller in
// a manager and checks that labeling a referenced Secret re-queues the proxy.
func TestCoderWorkspaceProxyCredentials_SecretWatchRequeues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "cwp-watch")
	tokenSecretObj := createSecret(ctx, t, tokenSecret(metav1.ObjectMeta{Name: "proxy-token", Namespace: namespace}, "token", testProxyTokenValue))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		Controller:             config.Controller{SkipNameValidation: ptrTo(true)},
	})
	require.NoError(t, err)
	r := &controller.CoderWorkspaceProxyReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
	require.NoError(t, r.SetupWithManager(mgr))
	mgrCtx, cancel := context.WithCancel(ctx)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-mgrDone)
	})

	workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", coderv1alpha1.WorkspaceProxySpec{
		PrimaryAccessURL:           testCoderOrigin,
		ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
	})
	require.Eventually(t, func() bool {
		condition := meta.FindStatusCondition(getWorkspaceProxy(ctx, t, workspaceProxy).Status.Conditions, "CredentialsReady")
		return condition != nil && condition.Reason == credentialsecret.ReasonNotOptedIn
	}, 30*time.Second, 100*time.Millisecond)

	// Label first (the label alone is not consent), then annotate.
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(tokenSecretObj), tokenSecretObj))
	tokenSecretObj.Labels = map[string]string{credentialsecret.LabelKey: credentialsecret.LabelValue}
	require.NoError(t, k8sClient.Update(ctx, tokenSecretObj))
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(tokenSecretObj), tokenSecretObj))
	tokenSecretObj.Annotations = map[string]string{credentialsecret.DestinationsAnnotation: testCoderOrigin}
	require.NoError(t, k8sClient.Update(ctx, tokenSecretObj))

	require.Eventually(t, func() bool {
		condition := meta.FindStatusCondition(getWorkspaceProxy(ctx, t, workspaceProxy).Status.Conditions, "CredentialsReady")
		_, exists := getProxyDeployment(ctx, t, workspaceProxy)
		return condition != nil && condition.Status == metav1.ConditionTrue && exists
	}, 30*time.Second, 100*time.Millisecond)
}

func TestCoderWorkspaceProxyCredentials_InvalidURLReportsMalformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec func(namespace string) coderv1alpha1.WorkspaceProxySpec
	}{
		{
			name: "primary access URL without scheme",
			spec: func(string) coderv1alpha1.WorkspaceProxySpec {
				return coderv1alpha1.WorkspaceProxySpec{
					PrimaryAccessURL:           "coder.example.com",
					ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
				}
			},
		},
		{
			name: "no token reference",
			spec: func(string) coderv1alpha1.WorkspaceProxySpec {
				return coderv1alpha1.WorkspaceProxySpec{PrimaryAccessURL: testCoderOrigin}
			},
		},
		{
			name: "bootstrap coderURL with another scheme",
			spec: func(string) coderv1alpha1.WorkspaceProxySpec {
				return coderv1alpha1.WorkspaceProxySpec{
					PrimaryAccessURL: testCoderOrigin,
					Bootstrap: &coderv1alpha1.ProxyBootstrapSpec{
						CoderURL:             "ftp://coder.example.com",
						CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
					},
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			namespace := createTestNamespace(ctx, t, "cwp-badurl")
			createSecret(ctx, t, tokenSecret(consentedObjectMeta("proxy-token", namespace, testCoderOrigin), "token", testProxyTokenValue))
			workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", tt.spec(namespace))
			bootstrapClient := &fakeBootstrapClient{response: coderbootstrap.RegisterWorkspaceProxyResponse{ProxyToken: testGeneratedToken}}
			r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
			requireProxyFailedStatic(ctx, t, r, bootstrapClient, workspaceProxy, credentialsecret.ReasonMalformed)
		})
	}
}
