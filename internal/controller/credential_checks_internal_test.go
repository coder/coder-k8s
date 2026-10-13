package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

func TestCheckReservedSettings(t *testing.T) {
	t.Parallel()

	reserved := reservedSettings{envNames: []string{"CODER_URL", "CODER_PROVISIONER_DAEMON_KEY"}, flags: []string{"url", "key"}}
	tests := []struct {
		name string
		env  []corev1.EnvVar
		args []string
		want string
	}{
		{name: "nothing set"},
		{name: "reserved env", env: []corev1.EnvVar{{Name: "CODER_URL", Value: "x"}}, want: "spec.extraEnv sets CODER_URL, which the operator sets itself. Remove the entry."},
		{name: "two dashes", args: []string{"--url", "x"}, want: "spec.extraArgs passes --url, which the operator sets itself. Remove the entry."},
		{name: "two dashes with value", args: []string{"--key=x"}, want: "spec.extraArgs passes --key, which the operator sets itself. Remove the entry."},
		{name: "one dash", args: []string{"-url"}, want: "spec.extraArgs passes --url, which the operator sets itself. Remove the entry."},
		{name: "one dash with value", args: []string{"-key=x"}, want: "spec.extraArgs passes --key, which the operator sets itself. Remove the entry."},
		{name: "three dashes are not a flag form", args: []string{"---url"}},
		{name: "longer flag", args: []string{"--keyless", "--url-prefix=x"}},
		{name: "positional value", args: []string{"url", "key=x"}},
		{name: "reserved after allowed", args: []string{"--verbose", "--url=x"}, want: "spec.extraArgs passes --url, which the operator sets itself. Remove the entry."},
		{name: "env names match without case", env: []corev1.EnvVar{{Name: "coder_url", Value: "x"}}, want: "spec.extraEnv sets coder_url, which the operator sets itself. Remove the entry."},
		{name: "env names match mixed case", env: []corev1.EnvVar{{Name: "Coder_Url", Value: "x"}}, want: "spec.extraEnv sets Coder_Url, which the operator sets itself. Remove the entry."},
		{
			name: "flag hidden in an env reference",
			env:  []corev1.EnvVar{{Name: "X", Value: "--url=https://evil.example.com"}},
			args: []string{"$(X)"},
			want: "spec.extraArgs passes --url, which the operator sets itself. Remove the entry.",
		},
		{
			name: "flag built from two env references",
			env:  []corev1.EnvVar{{Name: "A", Value: "--"}, {Name: "B", Value: "$(A)key"}},
			args: []string{"$(B)=x"},
			want: "spec.extraArgs passes --key, which the operator sets itself. Remove the entry.",
		},
		{
			name: "reference to a valueFrom variable",
			env:  []corev1.EnvVar{{Name: "S", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}}},
			args: []string{"$(S)"},
			want: "spec.extraArgs[0] uses $(S), whose value the operator cannot check for flags it sets itself. Write the value in the arg, or escape the reference as $$(S).",
		},
		{
			name: "reference to an operator variable",
			args: []string{"--verbose", "$(CODER_URL)"},
			want: "spec.extraArgs[1] uses $(CODER_URL), whose value the operator cannot check for flags it sets itself. Write the value in the arg, or escape the reference as $$(CODER_URL).",
		},
		{
			name: "valueFrom reference inside a flag name",
			env:  []corev1.EnvVar{{Name: "S", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}}},
			args: []string{"--u$(S)"},
			want: "spec.extraArgs[0] uses $(S), whose value the operator cannot check for flags it sets itself. Write the value in the arg, or escape the reference as $$(S).",
		},
		{
			name: "valueFrom reference in a flag value passes",
			env:  []corev1.EnvVar{{Name: "S", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}}},
			args: []string{"--git-token=$(S)", "--x=$(CODER_URL)", "value-$(S)"},
		},
		{
			name: "env value with a valueFrom reference after a prefix stays unknown",
			env: []corev1.EnvVar{
				{Name: "S", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}},
				{Name: "T", Value: "--u$(S)"},
			},
			args: []string{"$(T)"},
			want: "spec.extraArgs[0] uses $(T), whose value the operator cannot check for flags it sets itself. Write the value in the arg, or escape the reference as $$(T).",
		},
		{
			name: "literal reference before a valueFrom reference still matches",
			env: []corev1.EnvVar{
				{Name: "F", Value: "--url="},
				{Name: "S", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}},
			},
			args: []string{"$(F)$(S)"},
			want: "spec.extraArgs passes --url, which the operator sets itself. Remove the entry.",
		},
		{
			name: "env value that references a valueFrom variable",
			env: []corev1.EnvVar{
				{Name: "S", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
				{Name: "T", Value: "prefix-$(S)"},
			},
			args: []string{"$(T)"},
			want: "spec.extraArgs[0] uses $(T), whose value the operator cannot check for flags it sets itself. Write the value in the arg, or escape the reference as $$(T).",
		},
		{name: "escaped reference passes", env: []corev1.EnvVar{{Name: "X", Value: "--url=x"}}, args: []string{"$$(X)"}},
		{name: "undefined reference stays literal", args: []string{"$(UNDEFINED)", "$(unclosed"}},
		{name: "harmless reference passes", env: []corev1.EnvVar{{Name: "LEVEL", Value: "debug"}}, args: []string{"--log-level=$(LEVEL)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkReservedSettings(tt.env, tt.args, reserved)
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			credErr, ok := asCredentialError(err)
			require.True(t, ok, "expected a credential error, got %v", err)
			require.Equal(t, reasonReservedSettingOverride, credErr.Reason)
			require.Equal(t, tt.want, credErr.Message)
		})
	}
}

func TestCredentialSecretIndexes(t *testing.T) {
	t.Parallel()

	extraEnv := []corev1.EnvVar{
		{Name: "PLAIN", Value: "x"},
		{Name: "A", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "env-secret"}, Key: "k"}}},
		{Name: "B", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "env-secret"}, Key: "k2"}}},
		{Name: "C", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "a-configmap"}, Key: "k"}}},
	}

	direct := &coderv1alpha1.CoderWorkspaceProxy{Spec: coderv1alpha1.WorkspaceProxySpec{
		ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "direct-token"},
		ExtraEnv:                   extraEnv,
	}}
	direct.Name = "direct"
	require.Equal(t, []string{"direct-token", "env-secret"}, indexWorkspaceProxyBySecretNames(direct))

	bootstrapped := &coderv1alpha1.CoderWorkspaceProxy{Spec: coderv1alpha1.WorkspaceProxySpec{
		Bootstrap: &coderv1alpha1.ProxyBootstrapSpec{CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: "bootstrap"}},
	}}
	bootstrapped.Name = "boot"
	require.Equal(t, []string{"boot-proxy-token", "bootstrap"}, indexWorkspaceProxyBySecretNames(bootstrapped))
	bootstrapped.Spec.Bootstrap.GeneratedTokenSecretName = "named-token"
	require.Equal(t, []string{"bootstrap", "named-token"}, indexWorkspaceProxyBySecretNames(bootstrapped))

	provisioner := &coderv1alpha1.CoderProvisioner{Spec: coderv1alpha1.CoderProvisionerSpec{ExtraEnv: extraEnv}}
	provisioner.Name = "prov"
	require.Equal(t, []string{"env-secret", "prov-provisioner-key"}, indexProvisionerBySecretNames(provisioner))
	provisioner.Spec.Key.SecretName = "custom-key"
	require.Equal(t, []string{"custom-key", "env-secret"}, indexProvisionerBySecretNames(provisioner))

	require.Nil(t, indexWorkspaceProxyBySecretNames(&corev1.Secret{}))
	require.Nil(t, indexProvisionerBySecretNames(&corev1.Secret{}))
}

func TestReconcileRequestsForSecret(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, coderv1alpha1.AddToScheme(scheme))

	envRef := func(secret string) []corev1.EnvVar {
		return []corev1.EnvVar{{Name: "A", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: "k",
		}}}}
	}
	objects := []runtime.Object{
		&coderv1alpha1.CoderWorkspaceProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "uses", Namespace: "ns"},
			Spec:       coderv1alpha1.WorkspaceProxySpec{ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "shared"}},
		},
		&coderv1alpha1.CoderWorkspaceProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns"},
			Spec:       coderv1alpha1.WorkspaceProxySpec{ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "unrelated"}},
		},
		&coderv1alpha1.CoderWorkspaceProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "other-ns"},
			Spec:       coderv1alpha1.WorkspaceProxySpec{ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "shared"}},
		},
		&coderv1alpha1.CoderProvisioner{
			ObjectMeta: metav1.ObjectMeta{Name: "uses", Namespace: "ns"},
			Spec:       coderv1alpha1.CoderProvisionerSpec{ExtraEnv: envRef("shared")},
		},
		&coderv1alpha1.CoderProvisioner{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns"},
			Spec:       coderv1alpha1.CoderProvisionerSpec{ExtraEnv: envRef("unrelated")},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objects...).
		WithIndex(&coderv1alpha1.CoderWorkspaceProxy{}, credentialSecretNamesFieldIndex, indexWorkspaceProxyBySecretNames).
		WithIndex(&coderv1alpha1.CoderProvisioner{}, credentialSecretNamesFieldIndex, indexProvisionerBySecretNames).
		Build()

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "ns", Labels: map[string]string{credentialsecret.LabelKey: "true"}}}
	want := []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "uses", Namespace: "ns"}}}

	proxyReconciler := &CoderWorkspaceProxyReconciler{Client: c, Scheme: scheme}
	require.Equal(t, want, proxyReconciler.reconcileRequestsForSecret(context.Background(), secret))
	provisionerReconciler := &CoderProvisionerReconciler{Client: c, Scheme: scheme}
	require.Equal(t, want, provisionerReconciler.reconcileRequestsForSecret(context.Background(), secret))

	require.Empty(t, proxyReconciler.reconcileRequestsForSecret(context.Background(), &corev1.ConfigMap{}))
}
