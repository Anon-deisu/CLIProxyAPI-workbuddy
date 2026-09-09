package cliproxy

import (
	"context"
	"reflect"
	"testing"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestBuilderBuildInjectsPluginHostScheduler(t *testing.T) {
	host := pluginhost.New()
	service, errBuild := NewBuilder().
		WithConfig(&config.Config{AuthDir: t.TempDir()}).
		WithConfigPath(t.TempDir() + "/config.yaml").
		WithPluginHost(host).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}

	got := pluginSchedulerFromManager(t, service.coreManager)
	if got != host {
		t.Fatalf("plugin scheduler = %p, want host %p", got, host)
	}
}

func TestServiceSyncPluginRuntimeConfigInjectsPluginHostScheduler(t *testing.T) {
	host := pluginhost.New()
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		pluginHost:  host,
	}

	if ok := service.syncPluginRuntimeConfig(context.Background()); !ok {
		t.Fatal("syncPluginRuntimeConfig() = false, want true")
	}

	got := pluginSchedulerFromManager(t, service.coreManager)
	if got != host {
		t.Fatalf("plugin scheduler = %p, want host %p", got, host)
	}
}

func TestServiceSyncPluginRuntimeConfigClearsPluginSchedulerWithoutHost(t *testing.T) {
	host := pluginhost.New()
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		pluginHost:  host,
	}
	service.coreManager.SetPluginScheduler(host)
	service.pluginHost = nil

	if ok := service.syncPluginRuntimeConfig(context.Background()); ok {
		t.Fatal("syncPluginRuntimeConfig() = true, want false")
	}

	got := pluginSchedulerFromManager(t, service.coreManager)
	if got != nil {
		t.Fatalf("plugin scheduler = %p, want nil", got)
	}
}

func TestServiceConfigCommitPreservesRuntimeConcurrency(t *testing.T) {
	initial := &config.Config{}
	initial.Routing.AuthConcurrencyLimit = new(2)
	fingerprintEnabled := true
	initial.Routing.CodexFingerprintConvergence = &fingerprintEnabled
	service := &Service{cfg: initial}
	reloaded := &config.Config{Debug: true}
	commit := service.commitConfigUpdate(reloaded)
	if commit.cfg.Routing.AuthConcurrencyLimit == nil || *commit.cfg.Routing.AuthConcurrencyLimit != 2 {
		t.Fatalf("committed limit=%v, want 2", commit.cfg.Routing.AuthConcurrencyLimit)
	}
	if commit.cfg.Routing.CodexFingerprintConvergence == nil || !*commit.cfg.Routing.CodexFingerprintConvergence {
		t.Fatalf("committed fingerprint convergence=%v, want true", commit.cfg.Routing.CodexFingerprintConvergence)
	}
	if !commit.cfg.Debug || reloaded.Routing.AuthConcurrencyLimit != nil || reloaded.Routing.CodexFingerprintConvergence != nil {
		t.Fatal("reload fields lost or caller's config mutated")
	}
	*initial.Routing.AuthConcurrencyLimit = 9
	if *commit.cfg.Routing.AuthConcurrencyLimit != 2 {
		t.Fatal("inherited concurrency shares the previous config pointer")
	}
	*initial.Routing.CodexFingerprintConvergence = false
	if *commit.cfg.Routing.CodexFingerprintConvergence != true {
		t.Fatal("inherited fingerprint convergence shares the previous config pointer")
	}
	previous := commit.cfg
	reloadedWithLimit := &config.Config{}
	reloadedWithLimit.Routing.AuthConcurrencyLimit = new(3)
	commit = service.commitConfigUpdate(reloadedWithLimit)
	if commit.cfg.Routing.AuthConcurrencyLimit == nil || *commit.cfg.Routing.AuthConcurrencyLimit != 3 || commit.cfg.Routing.CodexFingerprintConvergence == nil || !*commit.cfg.Routing.CodexFingerprintConvergence {
		t.Fatalf("reload with explicit concurrency lost runtime fields: routing=%+v", commit.cfg.Routing)
	}
	if reloadedWithLimit.Routing.CodexFingerprintConvergence != nil || commit.cfg.Routing.CodexFingerprintConvergence == previous.Routing.CodexFingerprintConvergence || commit.cfg.Routing.AuthConcurrencyLimit == reloadedWithLimit.Routing.AuthConcurrencyLimit {
		t.Fatal("reload with explicit concurrency mutated the caller or shares runtime pointers")
	}
	unlimited := &config.Config{}
	unlimited.Routing.AuthConcurrencyLimit = new(0)
	service.commitConfigUpdate(unlimited)
	commit = service.commitConfigUpdate(&config.Config{})
	if commit.cfg.Routing.AuthConcurrencyLimit == nil || *commit.cfg.Routing.AuthConcurrencyLimit != 0 {
		t.Fatal("explicit unlimited setting was not preserved")
	}
	disabled := false
	explicit := &config.Config{}
	explicit.Routing.CodexFingerprintConvergence = &disabled
	commit = service.commitConfigUpdate(explicit)
	if commit.cfg.Routing.CodexFingerprintConvergence == nil || *commit.cfg.Routing.CodexFingerprintConvergence {
		t.Fatalf("explicit false convergence = %v, want false", commit.cfg.Routing.CodexFingerprintConvergence)
	}
	if commit.cfg == explicit || commit.cfg.Routing.CodexFingerprintConvergence == explicit.Routing.CodexFingerprintConvergence {
		t.Fatal("explicit runtime config shares the caller pointer")
	}
	reloadedAfterFalse := &config.Config{}
	commit = service.commitConfigUpdate(reloadedAfterFalse)
	if commit.cfg.Routing.CodexFingerprintConvergence == nil || *commit.cfg.Routing.CodexFingerprintConvergence {
		t.Fatalf("convergence after explicit false reload = %v, want false", commit.cfg.Routing.CodexFingerprintConvergence)
	}
	if reloadedAfterFalse.Routing.CodexFingerprintConvergence != nil {
		t.Fatal("reload after explicit false mutated the caller config")
	}
}

func TestServiceConfigCommitPreservesConvergenceWithoutConcurrency(t *testing.T) {
	for _, state := range []struct {
		name     string
		previous *bool
		value    *bool
		want     *bool
	}{
		{name: "unset"},
		{name: "enabled", value: new(true), want: new(true)},
		{name: "inherit-enabled", previous: new(true), want: new(true)},
		{name: "disabled", previous: new(true), value: new(false), want: new(false)},
		{name: "inherit-disabled", previous: new(false), want: new(false)},
		{name: "reenabled", previous: new(false), value: new(true), want: new(true)},
	} {
		t.Run(state.name, func(t *testing.T) {
			previous := &config.Config{}
			previous.Routing.CodexFingerprintConvergence = state.previous
			service := &Service{cfg: previous}
			cfg := &config.Config{Debug: true}
			cfg.Routing.CodexFingerprintConvergence = state.value
			before := cfg.CloneForRuntime()
			commit := service.commitConfigUpdate(cfg)
			if commit.cfg == nil || commit.cfg == cfg || commit.cfg == previous || service.cfg != commit.cfg {
				t.Fatal("service did not commit an independent config snapshot")
			}
			if !commit.cfg.Debug || !reflect.DeepEqual(commit.cfg.Routing.CodexFingerprintConvergence, state.want) || commit.cfg.Routing.AuthConcurrencyLimit != nil {
				t.Fatalf("committed routing = %+v", commit.cfg.Routing)
			}
			if !reflect.DeepEqual(cfg, before) {
				t.Fatal("service commit changed the caller config")
			}
			if state.want != nil && (commit.cfg.Routing.CodexFingerprintConvergence == cfg.Routing.CodexFingerprintConvergence || commit.cfg.Routing.CodexFingerprintConvergence == previous.Routing.CodexFingerprintConvergence) {
				t.Fatal("committed convergence shares the caller or previous config pointer")
			}
		})
	}
}

func pluginSchedulerFromManager(t *testing.T, manager *coreauth.Manager) *pluginhost.Host {
	t.Helper()
	if manager == nil {
		t.Fatal("manager = nil")
	}
	value := reflect.ValueOf(manager).Elem().FieldByName("pluginScheduler")
	if !value.IsValid() {
		t.Fatal("pluginScheduler field not found")
	}
	scheduler := reflect.NewAt(value.Type(), unsafe.Pointer(value.UnsafeAddr())).Elem().Interface()
	if scheduler == nil {
		return nil
	}
	host, ok := scheduler.(*pluginhost.Host)
	if !ok {
		t.Fatalf("pluginScheduler type = %T, want *pluginhost.Host", scheduler)
	}
	return host
}
