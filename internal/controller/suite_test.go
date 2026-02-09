package controller_test

import (
	"fmt"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

var (
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	scheme    *runtime.Scheme
)

func TestMain(m *testing.M) {
	scheme = runtime.NewScheme()

	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(fmt.Errorf("assertion failed: add client-go scheme: %w", err))
	}
	if err := coderv1alpha1.AddToScheme(scheme); err != nil {
		panic(fmt.Errorf("assertion failed: add coderv1alpha1 scheme: %w", err))
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{"../../config/crd/bases"},
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		panic(fmt.Errorf("assertion failed: envtest start: %w", err))
	}
	if cfg == nil {
		panic(fmt.Errorf("assertion failed: envtest config must not be nil"))
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(fmt.Errorf("assertion failed: create k8s client: %w", err))
	}
	if k8sClient == nil {
		panic(fmt.Errorf("assertion failed: k8s client must not be nil"))
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		panic(fmt.Errorf("assertion failed: envtest stop: %w", err))
	}

	os.Exit(code)
}
