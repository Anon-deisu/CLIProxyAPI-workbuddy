package helps

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var errCodexEgressChanged = errors.New("Codex account proxy binding changed; retry with a fresh request")

// CodexEgressManager retains fixed proxy bindings across executor reloads.
type CodexEgressManager struct {
	mu                  sync.Mutex
	source              func() []*cliproxyauth.Auth
	cfg                 *config.Config
	closed              bool
	egress              map[string]*codexEgressBinding
	egressGeneration    uint64
	savedISPBindings    map[string]string
	savedISPTopology    []string
	staticISPFile       string
	staticISPStoreError error
	egressError         error
}

func NewCodexEgressManager(source func() []*cliproxyauth.Auth) *CodexEgressManager {
	return &CodexEgressManager{source: source, egress: make(map[string]*codexEgressBinding)}
}

func (k *CodexEgressManager) Configure(cfg *config.Config) error {
	auths := k.source()
	k.mu.Lock()
	var connections []io.Closer
	defer func() { k.mu.Unlock(); closeCodexEgressConnections(connections) }()
	if k.closed {
		return errCodexEgressChanged
	}
	connections, k.egressError = k.reconcileEgressLocked(cfg, auths)
	if k.egressError != nil {
		return k.egressError
	}
	k.cfg = cfg.CloneForRuntime()
	return nil
}

func (k *CodexEgressManager) Close() {
	k.mu.Lock()
	k.closed = true
	var connections []io.Closer
	for _, binding := range k.egress {
		for conn := range binding.connections {
			connections = append(connections, conn)
		}
	}
	clear(k.egress)
	k.mu.Unlock()
	closeCodexEgressConnections(connections)
}

func codexEgressIdentity(auth *cliproxyauth.Auth) string {
	accountID := ""
	for _, key := range []string{"account_id", "chatgpt_account_id"} {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			accountID = strings.TrimSpace(value)
			break
		}
	}
	subject, _ := auth.Metadata["email"].(string)
	for _, key := range []string{"access_token", "id_token"} {
		token, _ := auth.Metadata[key].(string)
		if claims, errParse := codexauth.ParseJWTToken(token); errParse == nil && claims.Sub != "" {
			subject = claims.Sub
			break
		}
	}
	return fmt.Sprintf("%s\x00%s\x00%s", accountID, strings.TrimRight(strings.TrimSpace(auth.Attributes["base_url"]), "/"), subject)
}

// Request snapshots never enter the credential manager or its persistence path.
type codexEgressBinding struct {
	keeper      *CodexEgressManager
	auth        *cliproxyauth.Auth
	identity    string
	nodeID      string
	proxyURL    string
	generation  uint64
	connections map[io.Closer]struct{}
}

type CodexStaticISPStatus struct {
	AuthID string `json:"auth_id"`
	NodeID string `json:"node_id"`
}

type codexStaticISPSnapshot struct {
	Nodes    []string          `json:"nodes"`
	Bindings map[string]string `json:"bindings"`
}

func codexStaticISPEligible(auth *cliproxyauth.Auth) bool {
	return auth != nil && auth.ID != "" && strings.EqualFold(auth.Provider, "codex") && auth.AuthKind() == cliproxyauth.AuthKindOAuth
}

func staticISPPool(cfg *config.Config) *config.CodexStaticISPConfig {
	if cfg != nil {
		return cfg.Routing.CodexStaticISP
	}
	return nil
}

func (k *CodexEgressManager) SetStaticISPBindingFile(path string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.staticISPFile = path
	raw, errRead := os.ReadFile(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil
	}
	if errRead != nil {
		k.staticISPStoreError = fmt.Errorf("read static ISP bindings: %w", errRead)
		return k.staticISPStoreError
	}
	var saved codexStaticISPSnapshot
	if errDecode := json.Unmarshal(raw, &saved); errDecode != nil || saved.Bindings == nil {
		k.staticISPStoreError = errors.New("invalid static ISP binding file")
		return k.staticISPStoreError
	}
	k.savedISPBindings, k.savedISPTopology = saved.Bindings, saved.Nodes
	return nil
}

func (k *CodexEgressManager) saveISPBindingsLocked(bindings map[string]string, nodes []string) error {
	if k.staticISPStoreError != nil {
		return k.staticISPStoreError
	}
	if maps.Equal(k.savedISPBindings, bindings) && slices.Equal(nodes, k.savedISPTopology) {
		return nil
	}
	if k.staticISPFile != "" {
		dir := filepath.Dir(k.staticISPFile)
		if errDir := os.MkdirAll(dir, 0700); errDir != nil {
			return fmt.Errorf("create static ISP runtime directory: %w", errDir)
		}
		f, errCreate := os.CreateTemp(dir, ".codex-isp-*")
		if errCreate != nil {
			return fmt.Errorf("create static ISP binding snapshot: %w", errCreate)
		}
		defer func() { _ = os.Remove(f.Name()) }()
		errWrite := json.NewEncoder(f).Encode(codexStaticISPSnapshot{Nodes: nodes, Bindings: bindings})
		if errWrite == nil {
			errWrite = f.Sync()
		}
		errClose := f.Close()
		if errWrite != nil {
			return fmt.Errorf("write static ISP bindings: %w", errWrite)
		}
		if errClose != nil {
			return fmt.Errorf("close static ISP bindings: %w", errClose)
		}
		if errRename := os.Rename(f.Name(), k.staticISPFile); errRename != nil {
			return fmt.Errorf("replace static ISP bindings: %w", errRename)
		}
	}
	k.savedISPBindings = maps.Clone(bindings)
	k.savedISPTopology = slices.Clone(nodes)
	return nil
}

// reconcileEgressLocked retains existing assignments and balances only on topology changes.
func (k *CodexEgressManager) reconcileEgressLocked(cfg *config.Config, auths []*cliproxyauth.Auth) ([]io.Closer, error) {
	pool := staticISPPool(cfg)
	nodes := map[string]string{}
	if pool != nil {
		if errValidate := pool.Validate(); errValidate != nil {
			return nil, errValidate
		}
		if pool.Enabled {
			for _, node := range pool.Nodes {
				proxyURL, _ := node.ProxyURL()
				nodes[node.ID] = proxyURL
			}
		}
	}
	accounts := make(map[string]*cliproxyauth.Auth, len(k.egress)+len(auths))
	for id, binding := range k.egress {
		accounts[id] = binding.auth
	}
	for _, auth := range auths {
		if codexStaticISPEligible(auth) {
			accounts[auth.ID] = auth
		}
	}
	ids := make([]string, 0, len(accounts))
	for id := range accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	nodeIDs := make([]string, 0, len(nodes))
	for id := range nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	assignments := map[string]string{}
	if len(nodes) > 0 {
		counts := make(map[string]int, len(nodes))
		for _, id := range ids {
			node := k.savedISPBindings[id]
			if old := k.egress[id]; old != nil && nodes[old.nodeID] != "" {
				node = old.nodeID
			}
			if nodes[node] != "" {
				assignments[id] = node
				counts[node]++
			}
		}
		least := func() string {
			chosen := nodeIDs[0]
			for _, id := range nodeIDs[1:] {
				if counts[id] < counts[chosen] {
					chosen = id
				}
			}
			return chosen
		}
		for _, id := range ids {
			if assignments[id] == "" {
				node := least()
				assignments[id] = node
				counts[node]++
			}
		}
		topologyChanged := !slices.Equal(nodeIDs, k.savedISPTopology)
		if topologyChanged {
			for {
				low, high := least(), nodeIDs[0]
				for _, node := range nodeIDs[1:] {
					if counts[node] > counts[high] {
						high = node
					}
				}
				if counts[high]-counts[low] <= 1 {
					break
				}
				for i := len(ids) - 1; i >= 0; i-- {
					if assignments[ids[i]] == high {
						assignments[ids[i]] = low
						counts[high]--
						counts[low]++
						break
					}
				}
			}
		}
		persisted := make(map[string]string, len(k.savedISPBindings)+len(assignments))
		maps.Copy(persisted, k.savedISPBindings)
		maps.Copy(persisted, assignments)
		if errSave := k.saveISPBindingsLocked(persisted, nodeIDs); errSave != nil {
			return nil, errSave
		}
	}
	var closeConnections []io.Closer
	for _, id := range ids {
		auth := accounts[id]
		identity := codexEgressIdentity(auth)
		node := assignments[id]
		proxyURL := nodes[node]
		old := k.egress[id]
		if old != nil && old.identity == identity && old.nodeID == node && old.proxyURL == proxyURL {
			old.auth = auth.Clone()
			continue
		}
		if old != nil {
			for conn := range old.connections {
				closeConnections = append(closeConnections, conn)
			}
		}
		k.egressGeneration++
		k.egress[id] = &codexEgressBinding{keeper: k, auth: auth.Clone(), identity: identity, nodeID: node, proxyURL: proxyURL, generation: k.egressGeneration, connections: make(map[io.Closer]struct{})}
	}
	return closeConnections, nil
}

func closeCodexEgressConnections(connections []io.Closer) {
	for _, conn := range connections {
		if errClose := conn.Close(); errClose != nil {
			log.Debug("Codex proxy binding connection close failed")
		}
	}
}

// BindEgress returns an immutable request-local routing snapshot, or rejects an obsolete one.
func (k *CodexEgressManager) BindEgress(auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if k == nil || !codexStaticISPEligible(auth) {
		return auth, nil
	}
	k.mu.Lock()
	var connections []io.Closer
	defer func() { k.mu.Unlock(); closeCodexEgressConnections(connections) }()
	if k.closed {
		return nil, errCodexEgressChanged
	}
	if k.egressError != nil {
		return nil, k.egressError
	}
	if binding, ok := auth.Runtime.(*codexEgressBinding); ok {
		if binding.keeper != k || k.egress[auth.ID] != binding {
			return nil, errCodexEgressChanged
		}
		return auth, nil
	}
	if binding := k.egress[auth.ID]; binding == nil || binding.identity != codexEgressIdentity(auth) {
		var errReconcile error
		connections, errReconcile = k.reconcileEgressLocked(k.cfg, []*cliproxyauth.Auth{auth})
		if errReconcile != nil {
			return nil, errReconcile
		}
	}
	return k.boundEgressLocked(auth), nil
}

func (k *CodexEgressManager) boundEgressLocked(auth *cliproxyauth.Auth) *cliproxyauth.Auth {
	if binding, ok := auth.Runtime.(*codexEgressBinding); ok {
		if binding.keeper != k || k.egress[auth.ID] != binding {
			return nil
		}
		return auth
	}
	binding := k.egress[auth.ID]
	if binding == nil {
		return auth
	}
	cloned := auth.Clone()
	cloned.Runtime = binding
	if binding.proxyURL != "" {
		cloned.ProxyURL = binding.proxyURL
	}
	return cloned
}

// CodexEgressFingerprint distinguishes successive bindings even if a node is later restored.
func CodexEgressFingerprint(auth *cliproxyauth.Auth) string {
	if auth != nil {
		if binding, ok := auth.Runtime.(*codexEgressBinding); ok {
			return fmt.Sprint(binding.generation)
		}
	}
	return ""
}

func (k *CodexEgressManager) TrackEgressConnection(auth *cliproxyauth.Auth, conn io.Closer) error {
	if k == nil || auth == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if binding, ok := auth.Runtime.(*codexEgressBinding); ok {
		if k.egress[auth.ID] != binding || k.closed {
			return errCodexEgressChanged
		}
		binding.connections[conn] = struct{}{}
	}
	return nil
}

func (k *CodexEgressManager) ReleaseEgressConnection(auth *cliproxyauth.Auth, conn io.Closer) {
	if k == nil || auth == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if binding, ok := auth.Runtime.(*codexEgressBinding); ok {
		delete(binding.connections, conn)
	}
}

func (k *CodexEgressManager) StaticISPBindings() []CodexStaticISPStatus {
	k.mu.Lock()
	defer k.mu.Unlock()
	result := make([]CodexStaticISPStatus, 0, len(k.egress))
	for id, binding := range k.egress {
		if binding.nodeID != "" {
			result = append(result, CodexStaticISPStatus{AuthID: id, NodeID: binding.nodeID})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AuthID < result[j].AuthID })
	return result
}

func (k *CodexEgressManager) StaticISPError() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.egressError != nil {
		return k.egressError.Error()
	}
	return ""
}

func (k *CodexEgressManager) RemoveAuth(id string) {
	k.mu.Lock()
	var connections []io.Closer
	if binding := k.egress[id]; binding != nil {
		for conn := range binding.connections {
			connections = append(connections, conn)
		}
		delete(k.egress, id)
	}
	k.mu.Unlock()
	closeCodexEgressConnections(connections)
}

type codexEgressTransport struct {
	base    http.RoundTripper
	manager *CodexEgressManager
	auth    *cliproxyauth.Auth
}

func (k *CodexEgressManager) WrapHTTPClient(client *http.Client, auth *cliproxyauth.Auth) *http.Client {
	if k == nil {
		return client
	}
	cloned := *client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cloned.Transport = &codexEgressTransport{base: base, manager: k, auth: auth}
	return &cloned
}

func (t *codexEgressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, errBinding := t.manager.BindEgress(t.auth); errBinding != nil {
		return nil, errBinding
	}
	return t.base.RoundTrip(req)
}
