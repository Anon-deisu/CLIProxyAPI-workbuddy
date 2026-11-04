package config

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCodexStaticISPConfigRoundTrip(t *testing.T) {
	pool := &CodexStaticISPConfig{Enabled: true, Nodes: []StaticISPNode{{ID: "isp-a", Host: "2001:db8::1", Port: 1080, Username: "user@name", Password: "p:/@ secret"}}}
	if err := pool.Validate(); err != nil {
		t.Fatal(err)
	}
	rawURL, err := pool.Nodes[0].ProxyURL()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if u.Scheme != "socks5" || u.Hostname() != "2001:db8::1" || u.User.Username() != "user@name" || password != pool.Nodes[0].Password {
		t.Fatal("proxy credentials or IPv6 host did not round trip")
	}
	cfg := &Config{Routing: RoutingConfig{PersistSettings: new(true), CodexStaticISP: pool}}
	cfg.CredentialInFlight = DefaultCredentialInFlightConfig()
	cfg.AccountScheduling = cfg.AccountSchedulingSnapshot()
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing.CodexStaticISP.Nodes[0].Password != password {
		t.Fatal("saved static ISP password was not restored")
	}
	encoded, _ := json.Marshal(loaded)
	if strings.Contains(string(encoded), password) || strings.Contains(string(encoded), `"password":`) {
		t.Fatal("static ISP password leaked into config JSON")
	}
	cloned := (&Config{}).CloneForReload(loaded)
	cloned.Routing.CodexStaticISP.Nodes[0].Password = "replacement"
	if loaded.Routing.CodexStaticISP.Nodes[0].Password != password {
		t.Fatal("reload shared static ISP nodes")
	}
	off := &Config{Routing: RoutingConfig{CodexStaticISP: &CodexStaticISPConfig{}}}
	if off.CloneForReload(loaded).Routing.CodexStaticISP.Enabled {
		t.Fatal("explicit disable was lost on reload")
	}
}

func TestCodexStaticISPConfigRejectsInvalidNodes(t *testing.T) {
	valid := StaticISPNode{ID: "isp-a", Scheme: "http", Host: "proxy.test", Port: 8080, Username: "u", Password: "secret"}
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		node := valid
		node.Scheme = scheme
		if _, err := node.ProxyURL(); err != nil {
			t.Fatalf("supported scheme %s cannot construct a proxy dialer: %v", scheme, err)
		}
	}
	for _, mutate := range []func(*StaticISPNode){
		func(n *StaticISPNode) { n.ID = "" },
		func(n *StaticISPNode) { n.Scheme = "ftp" },
		func(n *StaticISPNode) { n.Host = "http://user:secret@proxy.test/path" },
		func(n *StaticISPNode) { n.Port = 0 },
		func(n *StaticISPNode) { n.Port = 65536 },
		func(n *StaticISPNode) { n.Password = "" },
	} {
		node := valid
		mutate(&node)
		pool := CodexStaticISPConfig{Enabled: true, Nodes: []StaticISPNode{node}}
		if err := pool.Validate(); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid node accepted or error leaked credentials")
		}
	}
	if err := (&CodexStaticISPConfig{Nodes: []StaticISPNode{valid, valid}}).Validate(); err == nil {
		t.Fatal("duplicate node IDs accepted")
	}
	if err := (&CodexStaticISPConfig{Enabled: true}).Validate(); err != nil {
		t.Fatal("empty pool must preserve legacy routing")
	}
}
