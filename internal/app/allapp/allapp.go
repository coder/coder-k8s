// Package allapp composes the controller and aggregated API server app modes in one process.
//
// The MCP HTTP server is deliberately not part of this mode: it acts with the operator's authority and
// only runs when explicitly requested with --app=mcp-http and a bearer token file.
package allapp

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder-k8s/internal/app/apiserverapp"
	"github.com/coder/coder-k8s/internal/app/controllerapp"
	"github.com/coder/coder-k8s/internal/app/sharedscheme"
)

const (
	cacheSyncTimeout           = 30 * time.Second
	defaultCoderRequestTimeout = 30 * time.Second
)

var (
	newManager             = controllerapp.NewManager
	setupControllers       = controllerapp.SetupControllers
	setupProbes            = controllerapp.SetupProbes
	runAggregatedAPIServer = func(ctx context.Context, opts apiserverapp.Options) error {
		return apiserverapp.RunWithOptions(ctx, opts)
	}
)

var _ manager.LeaderElectionRunnable = nonLeaderRunnable{}

type nonLeaderRunnable struct {
	run func(context.Context) error
}

func (r nonLeaderRunnable) Start(ctx context.Context) error {
	if r.run == nil {
		return fmt.Errorf("assertion failed: runnable function must not be nil")
	}
	return r.run(ctx)
}

func (nonLeaderRunnable) NeedLeaderElection() bool {
	return false
}

// Run starts all app modes together using a shared controller-runtime manager/cache.
func Run(ctx context.Context, coderRequestTimeout time.Duration) error {
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}
	if coderRequestTimeout < 0 {
		return fmt.Errorf("assertion failed: coder request timeout must not be negative: %s", coderRequestTimeout)
	}

	requestTimeout := coderRequestTimeout
	if requestTimeout == 0 {
		requestTimeout = defaultCoderRequestTimeout
	}

	scheme := sharedscheme.New()
	if scheme == nil {
		return fmt.Errorf("assertion failed: scheme is nil after successful construction")
	}

	cfg := ctrl.GetConfigOrDie()
	if cfg == nil {
		return fmt.Errorf("assertion failed: config is nil after successful construction")
	}

	mgr, err := newManager(cfg, scheme)
	if err != nil {
		return err
	}
	if mgr == nil {
		return fmt.Errorf("assertion failed: manager is nil after successful construction")
	}

	if err := setupControllers(mgr); err != nil {
		return err
	}
	if err := setupProbes(mgr); err != nil {
		return err
	}

	if err := addRunnables(mgr, requestTimeout); err != nil {
		return err
	}

	return mgr.Start(ctx)
}

// addRunnables registers the non-controller runnables that share the manager's cache.
func addRunnables(mgr manager.Manager, requestTimeout time.Duration) error {
	if mgr == nil {
		return fmt.Errorf("assertion failed: manager must not be nil")
	}
	if requestTimeout <= 0 {
		return fmt.Errorf("assertion failed: request timeout must be positive: %s", requestTimeout)
	}

	if err := mgr.Add(nonLeaderRunnable{
		run: func(runnableCtx context.Context) error {
			if runnableCtx == nil {
				return fmt.Errorf("assertion failed: context must not be nil")
			}

			if err := waitForCacheSync(runnableCtx, mgr, "aggregated-apiserver"); err != nil {
				return err
			}

			managerClient := mgr.GetClient()
			if managerClient == nil {
				return fmt.Errorf("assertion failed: manager client is nil")
			}

			apiReader := mgr.GetAPIReader()
			if apiReader == nil {
				return fmt.Errorf("assertion failed: manager API reader is nil")
			}

			provider, err := coder.NewControlPlaneClientProvider(managerClient, apiReader, requestTimeout)
			if err != nil {
				return fmt.Errorf("build control plane client provider: %w", err)
			}
			if provider == nil {
				return fmt.Errorf("assertion failed: control plane client provider is nil after successful construction")
			}

			return runAggregatedAPIServer(runnableCtx, apiserverapp.Options{
				ClientProvider:      provider,
				CoderRequestTimeout: requestTimeout,
			})
		},
	}); err != nil {
		return fmt.Errorf("add aggregated-apiserver runnable: %w", err)
	}

	return nil
}

func waitForCacheSync(ctx context.Context, mgr manager.Manager, runnableName string) error {
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}
	if mgr == nil {
		return fmt.Errorf("assertion failed: manager must not be nil")
	}
	if runnableName == "" {
		return fmt.Errorf("assertion failed: runnable name must not be empty")
	}

	managerCache := mgr.GetCache()
	if managerCache == nil {
		return fmt.Errorf("assertion failed: manager cache is nil")
	}

	syncCtx, cancel := context.WithTimeout(ctx, cacheSyncTimeout)
	defer cancel()

	if synced := managerCache.WaitForCacheSync(syncCtx); !synced {
		return fmt.Errorf("cache did not sync within %s for %s", cacheSyncTimeout, runnableName)
	}

	return nil
}
