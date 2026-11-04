package management

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"gopkg.in/yaml.v3"
)

const staticISPPasswordPlaceholder = "__CPA_STATIC_ISP_PASSWORD_UNCHANGED__"

type staticISPConfigPatch struct {
	Enabled *bool `json:"enabled"`
	Nodes   *[]struct {
		ID       string  `json:"id"`
		Scheme   string  `json:"scheme"`
		Host     string  `json:"host"`
		Port     int     `json:"port"`
		Username string  `json:"username"`
		Password *string `json:"password"`
	} `json:"nodes"`
}

func (patch *staticISPConfigPatch) apply(previous *config.CodexStaticISPConfig) (*config.CodexStaticISPConfig, error) {
	pool := &config.CodexStaticISPConfig{}
	passwords := map[string]string{}
	if previous != nil {
		pool.Enabled = previous.Enabled
		pool.Nodes = append([]config.StaticISPNode(nil), previous.Nodes...)
		for _, node := range previous.Nodes {
			passwords[node.ID] = node.Password
		}
	}
	if patch.Enabled != nil {
		pool.Enabled = *patch.Enabled
	}
	if patch.Nodes != nil {
		pool.Nodes = make([]config.StaticISPNode, 0, len(*patch.Nodes))
		for _, input := range *patch.Nodes {
			node := config.StaticISPNode{ID: strings.TrimSpace(input.ID), Scheme: strings.TrimSpace(input.Scheme), Host: strings.TrimSpace(input.Host), Port: input.Port, Username: input.Username}
			node.Password = passwords[node.ID]
			if input.Password != nil {
				node.Password = *input.Password
			}
			pool.Nodes = append(pool.Nodes, node)
		}
	}
	return pool, pool.Validate()
}

// rewriteStaticISPPasswords preserves unrelated YAML and comments. Redacted exports
// use an explicit marker so importing them cannot silently replace saved credentials.
func rewriteStaticISPPasswords(raw []byte, previous *config.CodexStaticISPConfig, redact bool) ([]byte, error) {
	var doc yaml.Node
	if errParse := yaml.Unmarshal(raw, &doc); errParse != nil {
		return nil, fmt.Errorf("invalid config YAML")
	}
	var child func(*yaml.Node, string) *yaml.Node
	child = func(node *yaml.Node, key string) *yaml.Node {
		if node == nil {
			return nil
		}
		if node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		if node.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				value := node.Content[i+1]
				if value.Kind == yaml.AliasNode {
					value = value.Alias
				}
				return value
			}
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == "<<" {
				merged := node.Content[i+1]
				if merged.Kind == yaml.SequenceNode {
					for _, source := range merged.Content {
						if value := child(source, key); value != nil {
							return value
						}
					}
				} else if value := child(merged, key); value != nil {
					return value
				}
			}
		}
		return nil
	}
	if len(doc.Content) == 0 {
		return raw, nil
	}
	nodes := child(child(child(doc.Content[0], "account-scheduling"), "codex-static-isp"), "nodes")
	if nodes == nil {
		return raw, nil
	}
	if nodes.Kind == yaml.AliasNode {
		nodes = nodes.Alias
	}
	passwords := map[string]string{}
	if previous != nil {
		for _, node := range previous.Nodes {
			passwords[node.ID] = node.Password
		}
	}
	changed := false
	for _, node := range nodes.Content {
		password := child(node, "password")
		if password == nil {
			continue
		}
		if password.Kind == yaml.AliasNode {
			password = password.Alias
		}
		if redact && password.Value != "" {
			password.Value = staticISPPasswordPlaceholder
			password.Tag = "!!str"
			changed = true
		} else if !redact && password.Value == staticISPPasswordPlaceholder {
			id := child(node, "id")
			if id == nil || passwords[id.Value] == "" {
				return nil, fmt.Errorf("static ISP password placeholder has no matching saved node")
			}
			password.Value = passwords[id.Value]
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	return yaml.Marshal(&doc)
}
