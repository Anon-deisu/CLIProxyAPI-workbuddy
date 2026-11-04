package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"gopkg.in/yaml.v3"
)

func TestCloneForRuntimeNil(t *testing.T) {
	var cfg *Config
	if got := cfg.CloneForRuntime(); got != nil {
		t.Fatalf("CloneForRuntime() = %#v, want nil", got)
	}
	if got := cfg.CloneForReload(&Config{}); got != nil {
		t.Fatalf("CloneForReload() = %#v, want nil", got)
	}
}

func TestCodexQuotaRefreshSettings(t *testing.T) {
	for _, tc := range []struct {
		name, raw   string
		enabled     bool
		interval    time.Duration
		concurrency int
	}{
		{"defaults", "port: 8317\n", true, 5 * time.Minute, 2},
		{"configured", "codex:\n  quota-refresh: {enabled: true, interval: 2m, concurrency: 4}\n", true, 2 * time.Minute, 4},
		{"disabled", "codex:\n  quota-refresh: {enabled: false}\n", false, 5 * time.Minute, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			enabled, interval, concurrency := cfg.Codex.QuotaRefresh.Effective()
			if enabled != tc.enabled || interval != tc.interval || concurrency != tc.concurrency {
				t.Fatalf("settings = %v, %s, %d", enabled, interval, concurrency)
			}
			cloned := cfg.CloneForRuntime()
			if cfg.Codex.QuotaRefresh.Enabled != nil {
				*cloned.Codex.QuotaRefresh.Enabled = !*cloned.Codex.QuotaRefresh.Enabled
				if *cloned.Codex.QuotaRefresh.Enabled == *cfg.Codex.QuotaRefresh.Enabled {
					t.Fatal("clone shares enabled pointer")
				}
			}
		})
	}
	for _, setting := range []string{"interval: invalid", "interval: 0s", "interval: -1s", "concurrency: -1"} {
		if _, err := ParseConfigBytes([]byte("codex:\n  quota-refresh: {" + setting + "}\n")); err == nil {
			t.Fatalf("accepted invalid quota refresh setting: %s", setting)
		}
	}
}

func TestAccountSchedulingSnapshotAndParsing(t *testing.T) {
	cfg := &Config{Routing: RoutingConfig{PersistSettings: new(true), AuthConcurrencyLimit: new(2), ExoFreePort: new(18888), CodexStaticISP: &CodexStaticISPConfig{Nodes: []StaticISPNode{{ID: "isp", Host: "proxy.test", Port: 1080, Username: "user", Password: "saved-secret"}}}}}
	cfg.CredentialInFlight = DefaultCredentialInFlightConfig()
	cfg.AccountScheduling = cfg.AccountSchedulingSnapshot()
	*cfg.Routing.AuthConcurrencyLimit = 9
	*cfg.Routing.ExoFreePort = 18889
	cfg.Routing.CodexStaticISP.Nodes[0].Password = "runtime-secret"
	if *cfg.AccountScheduling.AuthConcurrencyLimit != 2 || *cfg.AccountScheduling.ExoFreePort != 18888 || cfg.AccountScheduling.CodexStaticISP.Nodes[0].Password != "saved-secret" {
		t.Fatal("runtime changes mutated the last saved snapshot")
	}
	raw, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	parsed, errParse := ParseConfigBytes(raw)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if *parsed.Routing.AuthConcurrencyLimit != 2 || *parsed.Routing.ExoFreePort != 18888 || parsed.Routing.CodexStaticISP.Nodes[0].Password != "saved-secret" || !*parsed.Routing.PersistSettings {
		t.Fatal("parsing did not restore the saved snapshot")
	}
	*parsed.Routing.AuthConcurrencyLimit = 4
	*parsed.Routing.ExoFreePort = 18890
	if *parsed.AccountScheduling.AuthConcurrencyLimit != 2 || *parsed.AccountScheduling.ExoFreePort != 18888 {
		t.Fatal("restored runtime fields share pointers with saved settings")
	}
	previous := parsed.CloneForRuntime()
	parsed.Routing.PersistSettings = new(false)
	parsed.Routing.AuthConcurrencyLimit = new(7)
	parsed.Routing.ExoFreePort = new(0)
	updated := parsed.CloneForReload(previous)
	if *updated.Routing.AuthConcurrencyLimit != 7 || *updated.Routing.ExoFreePort != 0 || *updated.Routing.PersistSettings {
		t.Fatal("runtime config commit replaced explicitly supplied panel changes")
	}
}

func TestAccountSchedulingRejectsInvalidSavedSettings(t *testing.T) {
	for _, setting := range []string{
		"degraded-port: 65536", "exo-free-port: -1", "exo-free-port: 65536", "auth-concurrency-limit: -1", "c2a-max-retry-count: -1",
		"c2a-max-retry-interval: -1", "c2a-max-retry-credentials: 1000001",
		"cache-token-rewrite: {enabled: true, min-percent: 101}",
	} {
		if _, errParse := ParseConfigBytes([]byte("account-scheduling:\n  persist-settings: true\n  " + setting + "\n")); errParse == nil {
			t.Fatalf("invalid saved settings accepted: %s", setting)
		}
	}
}

func TestAccountSchedulingIgnoresRemovedCodexSettings(t *testing.T) {
	raw := []byte(`codex:
  turn-state-probe-model: obsolete-probe
account-scheduling:
  persist-settings: true
  auth-concurrency-limit: 3
  codex-turn-state-enabled: true
  codex-auto-degrade: true
  codex-excel-routing-enabled: true
  codex-excel-disable-fallback: true
  codex-state-cliproxy: {enabled: true, gateway: invalid}
`)
	cfg, errParse := ParseConfigBytes(raw)
	if errParse != nil {
		t.Fatalf("obsolete settings must not prevent startup: %v", errParse)
	}
	if cfg.Routing.AuthConcurrencyLimit == nil || *cfg.Routing.AuthConcurrencyLimit != 3 {
		t.Fatal("supported settings were lost")
	}
	encoded, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, key := range []string{"turn-state-probe-model", "codex-turn-state-enabled", "codex-auto-degrade", "codex-excel-routing-enabled", "codex-excel-disable-fallback", "codex-state-cliproxy"} {
		if strings.Contains(string(encoded), key) {
			t.Errorf("removed setting is still serialized: %s", key)
		}
	}
}

func TestSavingConfigRemovesRetiredCodexSettings(t *testing.T) {
	raw := []byte(`# keep this deployment comment
codex:
  turn-state-probe-model: obsolete-probe
  identity-confuse: true
account-scheduling:
  persist-settings: true
  auth-concurrency-limit: 3
  codex-turn-state-enabled: true
  codex-auto-degrade: true
  codex-excel-routing-enabled: true
  codex-excel-disable-fallback: true
  codex-state-cliproxy: {enabled: true, password: retired-secret}
  custom-field: keep-me
`)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, raw, 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if errSave := SaveConfigPreserveComments(path, cfg); errSave != nil {
		t.Fatal(errSave)
	}
	saved, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	for _, key := range []string{"turn-state-probe-model", "codex-turn-state-enabled", "codex-auto-degrade", "codex-excel-routing-enabled", "codex-excel-disable-fallback", "codex-state-cliproxy", "retired-secret"} {
		if strings.Contains(string(saved), key) {
			t.Errorf("retired setting survived save: %s", key)
		}
	}
	for _, want := range []string{"# keep this deployment comment", "custom-field: keep-me", "auth-concurrency-limit: 3", "identity-confuse: true"} {
		if !strings.Contains(string(saved), want) {
			t.Errorf("unrelated setting or comment was removed: %s", want)
		}
	}
}

func TestAccountSchedulingFakeModelsRoundTrip(t *testing.T) {
	models := []FakeModelConfig{{Name: "fake-a", Responses: []string{"fixed A", "alternative A"}, InputTokens: 114514, OutputTokens: 1919810, CacheReadTokens: 325, CacheWriteTokens: 799, CharsPerSecond: 25}, {Name: "fake-b", Content: "fixed B", InputTokens: 1}}
	cfg := &Config{Routing: RoutingConfig{FakeModels: CloneFakeModels(models)}}
	cfg.CredentialInFlight = DefaultCredentialInFlightConfig()
	cfg.AccountScheduling = cfg.AccountSchedulingSnapshot()
	cfg.Routing.FakeModels[0].Responses[0] = "changed"
	if cfg.AccountScheduling.FakeModels[0].Responses[0] != "fixed A" {
		t.Fatal("snapshot shares fake model content with runtime")
	}
	raw, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	parsed, errParse := ParseConfigBytes(raw)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if !reflect.DeepEqual(parsed.Routing.FakeModels, models) {
		t.Fatalf("restored fake models = %#v, want %#v", parsed.Routing.FakeModels, models)
	}
	previous := parsed.CloneForRuntime()
	updated := (&Config{}).CloneForReload(previous)
	if !reflect.DeepEqual(updated.Routing.FakeModels, models) {
		t.Fatal("config reload discarded runtime fake models")
	}
	updated.Routing.FakeModels[0].Responses[0] = "changed after reload"
	if previous.Routing.FakeModels[0].Responses[0] != "fixed A" {
		t.Fatal("config reload shares response pools with previous config")
	}
	parsed.Routing.FakeModels = []FakeModelConfig{}
	cleared := parsed.CloneForReload(previous)
	if len(cleared.Routing.FakeModels) != 0 {
		t.Fatal("explicit empty list did not remove fake models")
	}
}

func TestAccountSchedulingRejectsInvalidFakeModels(t *testing.T) {
	for _, raw := range []string{
		"- {name: '', content: fixed, input-tokens: 1}",
		"- {name: fake, content: ' ', input-tokens: 1}",
		"- {name: fake, content: fixed, input-tokens: -1}",
		"- {name: fake, content: fixed, cache-read-tokens: -1}",
		"- {name: fake, content: fixed, cache-write-tokens: -1}",
		"- {name: fake, responses: []}",
		"- {name: fake, content: legacy, responses: []}",
		"- {name: fake, responses: ['A', ' ']}",
		"- {name: fake, responses: ['A'], chars-per-second: -1}",
		"- {name: fake, responses: ['A'], chars-per-second: 1.5}",
		"- {name: fake, responses: ['A'], input-tokens: 9007199254740991, output-tokens: 1}",
		"- {name: fake, content: fixed}\n    - {name: fake, content: other}",
	} {
		if _, errParse := ParseConfigBytes([]byte("account-scheduling:\n  fake-models:\n    " + raw + "\n")); errParse == nil {
			t.Fatalf("invalid fake models accepted: %s", raw)
		}
	}
}

func TestParseConfigBytes_AntigravitySensitiveWords(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`antigravity:
  sensitive-words:
    - "API"
    - "proxy"
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	want := []string{"API", "proxy"}
	if !reflect.DeepEqual(cfg.Antigravity.SensitiveWords, want) {
		t.Fatalf("Antigravity.SensitiveWords = %#v, want %#v", cfg.Antigravity.SensitiveWords, want)
	}
}

func TestParseConfigBytes_AntigravityConnectionPool(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`antigravity:
  connection-pool:
    enabled: false
    idle-conn-timeout: "15s"
    max-idle-conns-per-host: 4
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.Antigravity.ConnectionPool.Enabled == nil || *cfg.Antigravity.ConnectionPool.Enabled != false {
		t.Fatalf("Enabled = %v, want false", cfg.Antigravity.ConnectionPool.Enabled)
	}
	if cfg.Antigravity.ConnectionPool.IdleConnTimeout != "15s" {
		t.Fatalf("IdleConnTimeout = %q, want 15s", cfg.Antigravity.ConnectionPool.IdleConnTimeout)
	}
	if cfg.Antigravity.ConnectionPool.MaxIdleConnsPerHost == nil || *cfg.Antigravity.ConnectionPool.MaxIdleConnsPerHost != 4 {
		t.Fatalf("MaxIdleConnsPerHost = %v, want 4", cfg.Antigravity.ConnectionPool.MaxIdleConnsPerHost)
	}
}

func TestCloneForRuntimeDeepCopiesConfig(t *testing.T) {
	cfg := sampleCloneRuntimeConfig()

	clone := cfg.CloneForRuntime()
	if clone == nil {
		t.Fatal("CloneForRuntime() = nil")
	}
	if clone == cfg {
		t.Fatal("CloneForRuntime() returned original pointer")
	}

	mutateOriginalConfig(cfg)

	if clone.Home.Host != "home.local" {
		t.Fatalf("clone.Home.Host = %q, want home.local", clone.Home.Host)
	}
	if clone.APIKeys[0] != "client-key" {
		t.Fatalf("clone.APIKeys[0] = %q, want client-key", clone.APIKeys[0])
	}
	if clone.OAuthExcludedModels["codex"][0] != "hidden-model" {
		t.Fatalf("clone.OAuthExcludedModels[codex][0] = %q, want hidden-model", clone.OAuthExcludedModels["codex"][0])
	}
	if clone.OAuthModelAlias["codex"][0].Alias != "client-model" {
		t.Fatalf("clone.OAuthModelAlias[codex][0].Alias = %q, want client-model", clone.OAuthModelAlias["codex"][0].Alias)
	}
	if got := pluginRawScalar(t, clone.Plugins.Configs["sample"].Raw, "mode"); got != "first" {
		t.Fatalf("clone plugin raw mode = %q, want first", got)
	}
	if clone.OpenAICompatibility[0].Models[0].Thinking.Levels[0] != "low" {
		t.Fatalf("clone thinking level = %q, want low", clone.OpenAICompatibility[0].Models[0].Thinking.Levels[0])
	}
	if got := clone.Payload.Default[0].Params["object"].(map[string]any)["key"]; got != "value" {
		t.Fatalf("clone payload object key = %#v, want value", got)
	}

	clone.APIKeys[0] = "clone-client-key"
	clone.OAuthExcludedModels["codex"][0] = "clone-hidden-model"
	clone.OAuthModelAlias["codex"][0].Alias = "clone-client-model"
	clone.OpenAICompatibility[0].Models[0].Thinking.Levels[0] = "clone-low"
	clone.Payload.Default[0].Params["object"].(map[string]any)["key"] = "clone-value"
	plugin := clone.Plugins.Configs["sample"]
	setPluginRawScalar(t, &plugin.Raw, "mode", "third")
	clone.Plugins.Configs["sample"] = plugin

	if cfg.APIKeys[0] != "mutated-client-key" {
		t.Fatalf("cfg.APIKeys[0] = %q, want mutated-client-key", cfg.APIKeys[0])
	}
	if cfg.OAuthExcludedModels["codex"][0] != "mutated-hidden-model" {
		t.Fatalf("cfg.OAuthExcludedModels[codex][0] = %q, want mutated-hidden-model", cfg.OAuthExcludedModels["codex"][0])
	}
	if cfg.OAuthModelAlias["codex"][0].Alias != "mutated-client-model" {
		t.Fatalf("cfg.OAuthModelAlias[codex][0].Alias = %q, want mutated-client-model", cfg.OAuthModelAlias["codex"][0].Alias)
	}
	if got := pluginRawScalar(t, cfg.Plugins.Configs["sample"].Raw, "mode"); got != "second" {
		t.Fatalf("cfg plugin raw mode = %q, want second", got)
	}
	if cfg.OpenAICompatibility[0].Models[0].Thinking.Levels[0] != "mutated-low" {
		t.Fatalf("cfg thinking level = %q, want mutated-low", cfg.OpenAICompatibility[0].Models[0].Thinking.Levels[0])
	}
	if got := cfg.Payload.Default[0].Params["object"].(map[string]any)["key"]; got != "mutated-value" {
		t.Fatalf("cfg payload object key = %#v, want mutated-value", got)
	}
}

func TestCloneForReloadInheritsOnlySupportedRuntimeFields(t *testing.T) {
	previousLimit := 2
	previousConvergence := true
	previousRetryCount := 7
	previousCacheMin := 80.0
	previousCacheMax := 95.0
	previous := &Config{}
	previous.CacheTokenRewrite = &CacheTokenRewriteConfig{
		Enabled:    true,
		MinPercent: &previousCacheMin,
		MaxPercent: &previousCacheMax,
	}
	previous.Routing.AuthConcurrencyLimit = &previousLimit
	previous.Routing.CodexFingerprintConvergence = &previousConvergence
	previous.Routing.C2AMaxRetryCount = &previousRetryCount
	previous.Routing.C2AMaxRetryInterval = new(30)
	previous.Routing.SchedulerMode = "remaining-quota"
	previous.Routing.AutoDisableUnauthorized = true
	previous.Routing.AutoDisableQuota = true
	previous.Home.Enabled = true
	previous.CodexOptimizeMultiAgentV2 = true
	current := &Config{Debug: true}

	clone := current.CloneForReload(previous)
	if clone == current {
		t.Fatal("CloneForReload() returned the original pointer")
	}
	if clone.Routing.AuthConcurrencyLimit == nil || *clone.Routing.AuthConcurrencyLimit != previousLimit {
		t.Fatalf("inherited concurrency limit = %v, want %d", clone.Routing.AuthConcurrencyLimit, previousLimit)
	}
	if clone.Routing.CodexFingerprintConvergence == nil || !*clone.Routing.CodexFingerprintConvergence {
		t.Fatalf("inherited convergence = %v, want true", clone.Routing.CodexFingerprintConvergence)
	}
	want := &Config{
		SDKConfig: SDKConfig{CacheTokenRewrite: &CacheTokenRewriteConfig{
			Enabled:    true,
			MinPercent: new(80.0),
			MaxPercent: new(95.0),
		}},
		Debug: true,
		Routing: RoutingConfig{
			AuthConcurrencyLimit:        new(2),
			CodexFingerprintConvergence: new(true),
			C2AMaxRetryCount:            new(7),
			C2AMaxRetryInterval:         new(30),
		},
	}
	if !reflect.DeepEqual(clone, want) {
		t.Fatalf("reload lost current fields or inherited unsupported fields: clone=%#v want=%#v", clone, want)
	}
	if current.Routing.AuthConcurrencyLimit != nil || current.Routing.CodexFingerprintConvergence != nil {
		t.Fatal("inheriting runtime fields mutated the caller config")
	}

	previousLimit = 9
	previousConvergence = false
	previousCacheMin = 10
	previousCacheMax = 20
	if *clone.Routing.AuthConcurrencyLimit != 2 || !*clone.Routing.CodexFingerprintConvergence || *clone.CacheTokenRewrite.MinPercent != 80 || *clone.CacheTokenRewrite.MaxPercent != 95 {
		t.Fatal("inherited runtime fields share pointers with the previous config")
	}

	explicitLimit := 0
	explicitConvergence := false
	explicit := &Config{}
	explicit.Routing.AuthConcurrencyLimit = &explicitLimit
	explicit.Routing.CodexFingerprintConvergence = &explicitConvergence
	previousConvergence = true
	explicitClone := explicit.CloneForReload(previous)
	if explicitClone.Routing.AuthConcurrencyLimit == nil || *explicitClone.Routing.AuthConcurrencyLimit != 0 {
		t.Fatalf("explicit concurrency limit = %v, want 0", explicitClone.Routing.AuthConcurrencyLimit)
	}
	if explicitClone.Routing.CodexFingerprintConvergence == nil || *explicitClone.Routing.CodexFingerprintConvergence {
		t.Fatalf("explicit convergence = %v, want false", explicitClone.Routing.CodexFingerprintConvergence)
	}
	if explicit.Routing.AuthConcurrencyLimit == nil || *explicit.Routing.AuthConcurrencyLimit != 0 || explicit.Routing.CodexFingerprintConvergence == nil || *explicit.Routing.CodexFingerprintConvergence {
		t.Fatal("cloning explicit runtime fields mutated the caller config")
	}
	assertNoSharedRuntimeReferences(t, reflect.ValueOf(explicit), reflect.ValueOf(explicitClone), "Config")
	inheritedFalse := (&Config{}).CloneForReload(explicitClone)
	if inheritedFalse.Routing.CodexFingerprintConvergence == nil || *inheritedFalse.Routing.CodexFingerprintConvergence {
		t.Fatal("reload did not inherit explicit false")
	}
	assertNoSharedRuntimeReferences(t, reflect.ValueOf(explicitClone), reflect.ValueOf(inheritedFalse), "Config")

	withoutPrevious := current.CloneForReload(nil)
	if withoutPrevious == current || !reflect.DeepEqual(withoutPrevious, current) {
		t.Fatal("reload without a previous config did not preserve an independent snapshot")
	}
	if withoutPrevious.Routing.CodexFingerprintConvergence != nil || withoutPrevious.Routing.AuthConcurrencyLimit != nil {
		t.Fatal("reload invented unset runtime fields")
	}
}

func TestCloneForRuntimeDoesNotShareReferenceFields(t *testing.T) {
	cfg := sampleCloneRuntimeConfig()
	clone := cfg.CloneForRuntime()

	assertNoSharedRuntimeReferences(t, reflect.ValueOf(cfg), reflect.ValueOf(clone), "Config")
}

func TestWorkBuddyFingerprintConfigSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		previous, value, want *bool
	}{
		{name: "default-off"},
		{name: "enable", value: new(true), want: new(true)},
		{name: "inherit", previous: new(true), want: new(true)},
		{name: "disable", previous: new(true), value: new(false), want: new(false)},
		{name: "inherit-disabled", previous: new(false), want: new(false)},
		{name: "reenable", previous: new(false), value: new(true), want: new(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := &Config{Routing: RoutingConfig{WorkBuddyFingerprintConvergence: tc.previous}}
			current := &Config{Routing: RoutingConfig{WorkBuddyFingerprintConvergence: tc.value}}
			cloned := current.CloneForReload(previous)
			if !reflect.DeepEqual(cloned.Routing.WorkBuddyFingerprintConvergence, tc.want) {
				t.Fatalf("unexpected convergence snapshot: %+v", cloned.Routing)
			}
			assertNoSharedRuntimeReferences(t, reflect.ValueOf(previous), reflect.ValueOf(cloned), "Config")
			assertNoSharedRuntimeReferences(t, reflect.ValueOf(current), reflect.ValueOf(cloned), "Config")
			snapshot := cloned.AccountSchedulingSnapshot()
			restored := &Config{}
			snapshot.apply(restored)
			if !reflect.DeepEqual(restored.Routing.WorkBuddyFingerprintConvergence, tc.want) || restored.Routing.CodexFingerprintConvergence != nil {
				t.Fatal("account scheduling snapshot lost WorkBuddy state or changed Codex")
			}
			if tc.want != nil {
				*snapshot.WorkBuddyFingerprintConvergence = !*tc.want
				if *restored.Routing.WorkBuddyFingerprintConvergence != *tc.want || *cloned.Routing.WorkBuddyFingerprintConvergence != *tc.want {
					t.Fatal("saved settings share mutable pointers with runtime configs")
				}
			}
		})
	}
}

func sampleCloneRuntimeConfig() *Config {
	cacheStrict := true
	bypassStrict := false
	pluginEnabled := false
	cacheUserID := true

	return &Config{
		SDKConfig: SDKConfig{
			APIKeys: []string{"client-key"},
			Streaming: StreamingConfig{
				KeepAliveSeconds:      3,
				BootstrapRetries:      2,
				SendKeepAliveBeforeFT: true,
			},
		},
		Routing: RoutingConfig{
			AuthConcurrencyLimit:        new(2),
			CodexFingerprintConvergence: new(false),
		},
		Home: HomeConfig{
			Enabled: true,
			Host:    "home.local",
			Port:    8081,
			TLS: HomeTLSConfig{
				Enable:              true,
				ServerName:          "home.local",
				CACert:              "ca",
				ClientCert:          "cert",
				ClientKey:           "key",
				UseTargetServerName: true,
			},
		},
		Plugins: PluginsConfig{
			Enabled:      true,
			Dir:          "plugins",
			StoreSources: []string{"https://plugins.example/store.json"},
			Configs: map[string]PluginInstanceConfig{
				"sample": {
					Enabled:  &pluginEnabled,
					Priority: 10,
					Raw:      samplePluginRawNode("first"),
				},
			},
		},
		AntigravitySignatureCacheEnabled: &cacheStrict,
		AntigravitySignatureBypassStrict: &bypassStrict,
		GeminiKey: []GeminiKey{{
			APIKey:         "gemini-key",
			Models:         []GeminiModel{{Name: "gemini-upstream", Alias: "gemini-upstream-alias"}},
			Headers:        map[string]string{"X-Gemini": "one"},
			ExcludedModels: []string{"gemini-hidden"},
		}},
		CodexKey: []CodexKey{{
			APIKey:         "codex-key",
			Models:         []CodexModel{{Name: "codex-upstream", Alias: "codex-client"}},
			Headers:        map[string]string{"X-Codex": "one"},
			ExcludedModels: []string{"codex-hidden-key"},
		}},
		ClaudeKey: []ClaudeKey{{
			APIKey:         "claude-key",
			Models:         []ClaudeModel{{Name: "claude-upstream", Alias: "claude-client"}},
			Headers:        map[string]string{"X-Claude": "one"},
			ExcludedModels: []string{"claude-hidden"},
			Cloak: &CloakConfig{
				SensitiveWords: []string{"secret"},
				CacheUserID:    &cacheUserID,
			},
		}},
		OpenAICompatibility: []OpenAICompatibility{{
			Name:          "compat",
			APIKeyEntries: []OpenAICompatibilityAPIKey{{APIKey: "compat-key", ProxyURL: "http://proxy.local"}},
			Models: []OpenAICompatibilityModel{{
				Name:     "compat-upstream",
				Alias:    "compat-client",
				Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}},
			}},
			Headers: map[string]string{"X-Compat": "one"},
		}},
		VertexCompatAPIKey: []VertexCompatKey{{
			APIKey:         "vertex-key",
			Headers:        map[string]string{"X-Vertex": "one"},
			Models:         []VertexCompatModel{{Name: "vertex-upstream", Alias: "vertex-client"}},
			ExcludedModels: []string{"vertex-hidden"},
		}},
		OAuthExcludedModels: map[string][]string{
			"codex": {"hidden-model"},
		},
		OAuthModelAlias: map[string][]OAuthModelAlias{
			"codex": {{Name: "upstream-model", Alias: "client-model", Fork: true}},
		},
		Payload: PayloadConfig{
			Default: []PayloadRule{{
				Models: []PayloadModelRule{{
					Name:    "model-*",
					Headers: map[string]string{"X-Tier": "gold"},
					Match:   []map[string]any{{"tier": "gold"}},
					Exist:   []string{"$.messages"},
				}},
				Params: map[string]any{
					"object": map[string]any{"key": "value"},
					"array":  []any{"first", map[string]any{"nested": "value"}},
				},
			}},
			Filter: []PayloadFilterRule{{
				Models: []PayloadModelRule{{Name: "model-*"}},
				Params: []string{"$.secret"},
			}},
		},
	}
}

func mutateOriginalConfig(cfg *Config) {
	cfg.Home.Host = "mutated-home.local"
	cfg.APIKeys[0] = "mutated-client-key"
	cfg.OAuthExcludedModels["codex"][0] = "mutated-hidden-model"
	cfg.OAuthModelAlias["codex"][0].Alias = "mutated-client-model"
	cfg.OpenAICompatibility[0].Models[0].Thinking.Levels[0] = "mutated-low"
	cfg.Payload.Default[0].Params["object"].(map[string]any)["key"] = "mutated-value"
	plugin := cfg.Plugins.Configs["sample"]
	setPluginRawScalar(nil, &plugin.Raw, "mode", "second")
	cfg.Plugins.Configs["sample"] = plugin
}

func samplePluginRawNode(mode string) yaml.Node {
	modeValue := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: mode, Anchor: "modeAnchor"}
	return yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "enabled"},
			{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mode"},
			modeValue,
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mode-alias"},
			{Kind: yaml.AliasNode, Alias: modeValue},
		},
	}
}

func pluginRawScalar(t *testing.T, node yaml.Node, key string) string {
	t.Helper()
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i] != nil && node.Content[i].Value == key && node.Content[i+1] != nil {
			return node.Content[i+1].Value
		}
	}
	t.Fatalf("raw plugin node missing key %q", key)
	return ""
}

func setPluginRawScalar(t *testing.T, node *yaml.Node, key, value string) {
	if t != nil {
		t.Helper()
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i] != nil && node.Content[i].Value == key && node.Content[i+1] != nil {
			node.Content[i+1].Value = value
			return
		}
	}
	if t != nil {
		t.Fatalf("raw plugin node missing key %q", key)
	}
}

func assertNoSharedRuntimeReferences(t *testing.T, original, clone reflect.Value, path string) {
	t.Helper()
	if !original.IsValid() || !clone.IsValid() {
		return
	}
	if original.Kind() == reflect.Interface {
		if original.IsNil() || clone.IsNil() {
			return
		}
		assertNoSharedRuntimeReferences(t, original.Elem(), clone.Elem(), path)
		return
	}
	if original.Kind() != clone.Kind() {
		t.Fatalf("%s kind mismatch: %s != %s", path, original.Kind(), clone.Kind())
	}

	switch original.Kind() {
	case reflect.Pointer:
		if original.IsNil() || clone.IsNil() {
			return
		}
		if original.Pointer() == clone.Pointer() {
			t.Fatalf("%s shares pointer %x", path, original.Pointer())
		}
		assertNoSharedRuntimeReferences(t, original.Elem(), clone.Elem(), path+"->"+original.Type().Elem().String())
	case reflect.Map:
		if original.IsNil() || clone.IsNil() {
			return
		}
		if original.Pointer() == clone.Pointer() {
			t.Fatalf("%s shares map pointer %x", path, original.Pointer())
		}
		iter := original.MapRange()
		for iter.Next() {
			key := iter.Key()
			assertNoSharedRuntimeReferences(t, iter.Value(), clone.MapIndex(key), path+"["+keyForPath(key)+"]")
		}
	case reflect.Slice:
		if original.IsNil() || clone.IsNil() {
			return
		}
		if original.Pointer() == clone.Pointer() {
			t.Fatalf("%s shares slice pointer %x", path, original.Pointer())
		}
		for i := 0; i < original.Len(); i++ {
			assertNoSharedRuntimeReferences(t, original.Index(i), clone.Index(i), path+"[]")
		}
	case reflect.Struct:
		for i := 0; i < original.NumField(); i++ {
			field := original.Type().Field(i)
			assertNoSharedRuntimeReferences(t, original.Field(i), clone.Field(i), path+"."+field.Name)
		}
	}
}

func keyForPath(key reflect.Value) string {
	if key.Kind() == reflect.String {
		return key.String()
	}
	return key.Type().String()
}

func TestParseConfigBytes_ProxyConnectionPool(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("api-keys:\n  codex:\n    - name: pool-group\n      base-url: https://ollama.com/v1\n      connection-pool:\n        enabled: true\n        idle-conn-timeout: 15s\n        max-idle-conns-per-host: 4\n      keys:\n        - api-key: pool-key\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CodexKey) != 1 {
		t.Fatalf("codex entries = %d, want 1", len(cfg.CodexKey))
	}
	pool := cfg.CodexKey[0].ConnectionPool
	if pool == nil || pool.Enabled == nil || !*pool.Enabled || pool.IdleConnTimeout != "15s" || pool.MaxIdleConnsPerHost == nil || *pool.MaxIdleConnsPerHost != 4 {
		t.Fatalf("unexpected pool config: %+v", pool)
	}
	clone := cfg.CloneForRuntime()
	if clone.CodexKey[0].ConnectionPool == pool {
		t.Fatal("clone shares the pool block")
	}
	*clone.CodexKey[0].ConnectionPool.Enabled = false
	if !*cfg.CodexKey[0].ConnectionPool.Enabled {
		t.Fatal("clone shares enabled pointer")
	}
}
