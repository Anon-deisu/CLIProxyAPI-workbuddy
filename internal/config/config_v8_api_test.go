package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestV8AliasesIncludeReflectedLocalFields(t *testing.T) {
	aliases := make(map[string]string, len(v8Aliases))
	for _, alias := range v8Aliases {
		if previous, exists := aliases[alias.old]; exists {
			t.Fatalf("duplicate alias %s: %s and %s", alias.old, previous, alias.current)
		}
		aliases[alias.old] = alias.current
	}
	for _, path := range []configPath{
		{"oauth.providers.claude.tool-search", "upstream.claude.tool-search"},
		{"oauth.providers.claude.session-date-pin-days", "upstream.claude.session-date-pin-days"},
		{"oauth.providers.claude.header-defaults.native-entrypoints", "upstream.claude.header-defaults.native-entrypoints"},
		{"oauth.providers.codex.response-steering", "upstream.codex.response-steering"},
		{"oauth.providers.codex.optimize-multi-agent-v2", "client.codex.optimize-multi-agent-v2"},
	} {
		if got := aliases[path.old]; got != path.current {
			t.Errorf("alias %s = %q, want %q", path.old, got, path.current)
		}
	}
}

func TestV8KeyFamiliesIncludeLocalSchema(t *testing.T) {
	type localSchema struct {
		WorkBuddy []struct{} `yaml:"workbuddy-api-key,omitempty"`
		Cursor    []struct{} `yaml:"cursor-api-key"`
		Meta      []struct{} `yaml:"meta-api-key"`
		Qoder     []struct{} `yaml:"qoder-api-key"`
		Devin     []struct{} `yaml:"devin-api-key"`
		Client    []string   `yaml:"api-keys"`
		Runtime   []struct{} `yaml:"-"`
	}
	want := []configPath{
		{"workbuddy-api-key", "workbuddy"}, {"cursor-api-key", "cursor"},
		{"meta-api-key", "meta"}, {"qoder-api-key", "qoder"}, {"devin-api-key", "devin"},
	}
	if got := buildV8KeyFamilies(reflect.TypeOf(localSchema{})); !reflect.DeepEqual(got, want) {
		t.Fatalf("local API-key families = %#v, want %#v", got, want)
	}
}

func TestV8APIAliasRoundTripKeepsLocalProviderSettings(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(`upstream:
  claude: {tool-search: true, session-date-pin-days: 3}
oauth:
  providers:
    codex: {signin-with-chatgpt-identifier: ExampleClient, quota-refresh: {enabled: false}}
    devin: {}
  model-alias:
    workbuddy: [{name: test-model, alias: workbuddy-model}]
    cursor: [{name: test-model, alias: cursor-model}]
    meta: [{name: test-model, alias: meta-model}]
    qoder: [{name: test-model, alias: qoder-model}]
    devin: [{name: test-model, alias: devin-model}]
account-scheduling: {workbuddy-fingerprint-convergence: false}
api-keys:
  meta: [{keys: [{api-key: 'LLM|test-key'}]}]
`), &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	var before any
	if err := root.Decode(&before); err != nil {
		t.Fatal(err)
	}
	ProjectV8ConfigAliases(root, "oauth.providers.claude")
	if yamlPath(root, "oauth.providers.claude.tool-search") == nil || yamlPath(root, "upstream.claude.tool-search") != nil {
		t.Fatal("projection did not move the shared local fields into the historical subtree")
	}
	if err := NormalizeV8ConfigAliases(root); err != nil {
		t.Fatal(err)
	}
	var after any
	if err := root.Decode(&after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("alias projection and normalization changed local provider configuration")
	}
	data, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(data); err != nil {
		t.Fatalf("round-tripped local configuration is invalid: %v", err)
	}
}

func TestV8APIAliasNullResetAndCanonicalPrecedence(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("oauth: {providers: {claude: null}}\nupstream: {claude: {tool-search: false}}\n"), &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	if err := NormalizeV8ConfigAliases(root); err != nil {
		t.Fatal(err)
	}
	if node := yamlPath(root, "upstream.claude.tool-search"); node == nil || node.Tag != "!!bool" || node.Value != "false" {
		t.Fatal("historical null reset overwrote an explicit canonical value")
	}
	if node := yamlPath(root, "upstream.claude.session-date-pin-days"); node == nil || node.Tag != "!!null" {
		t.Fatal("historical null reset did not include a reflected local field")
	}
	if yamlPath(root, "oauth.providers.claude") != nil {
		t.Fatal("historical null container remained after normalization")
	}
}
