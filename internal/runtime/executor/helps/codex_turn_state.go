package helps

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

const (
	CodexTurnStateHeader           = "X-Codex-Turn-State"
	CodexTurnStateTTL              = time.Hour
	CodexTurnStateRefreshBefore    = 5 * time.Minute
	CodexTurnStateCheckInterval    = 45 * time.Second
	codexTurnStateDefaultBaseURL   = "https://chatgpt.com/backend-api/codex"
	codexTurnStateProbeModel       = "gpt-6-astra"
	codexTurnStateProbeConcurrency = 4
	codexTurnStateMetadataPath     = "client_metadata.x-codex-turn-state"
	codexTurnStatePersonalLength   = 292
	codexTurnStateTeamLength       = 332
	codexTurnStateLengthTolerance  = 4
)

// CodexTurnStateStatus exposes estimated validity without exposing the opaque state.
type CodexTurnStateStatus struct {
	AuthID           string     `json:"auth_id"`
	Name             string     `json:"name"`
	Status           string     `json:"status"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	Refreshing       bool       `json:"refreshing"`
	LastError        string     `json:"last_error,omitempty"`
	TokenLength      int        `json:"token_length,omitempty"`
	ObservedLength   int        `json:"observed_length,omitempty"`
}

type codexTurnStateEntry struct {
	auth           *cliproxyauth.Auth
	scope          string
	token          string
	expires        time.Time
	lastAttempt    time.Time
	lastError      string
	observedLength int
	refreshing     bool
	cancel         context.CancelFunc
}

// CodexTurnStateKeeper owns one in-memory state per auth record. It survives executor reloads.
type CodexTurnStateKeeper struct {
	mu               sync.Mutex
	source           func() []*cliproxyauth.Auth
	fetch            func(context.Context, *config.Config, *cliproxyauth.Auth) (string, error)
	updateScheduling func(context.Context, *cliproxyauth.Auth, bool) error
	cfg              *config.Config
	entries          map[string]*codexTurnStateEntry
	ctx              context.Context
	cancel           context.CancelFunc
	closed           bool
	wg               sync.WaitGroup
	slots            chan struct{}
}

func NewCodexTurnStateKeeper(source func() []*cliproxyauth.Auth) *CodexTurnStateKeeper {
	return &CodexTurnStateKeeper{
		source: source, fetch: fetchCodexTurnState,
		entries: make(map[string]*codexTurnStateEntry),
		slots:   make(chan struct{}, codexTurnStateProbeConcurrency),
	}
}

// SetSchedulingUpdater connects observations to the service's credential manager.
func (k *CodexTurnStateKeeper) SetSchedulingUpdater(update func(context.Context, *cliproxyauth.Auth, bool) error) {
	k.mu.Lock()
	k.updateScheduling = update
	k.mu.Unlock()
}

func (k *CodexTurnStateKeeper) autoDegradeEnabledLocked() bool {
	return k.ctx != nil && k.cfg.Routing.CodexAutoDegrade != nil && *k.cfg.Routing.CodexAutoDegrade
}

// Configure starts or stops collection independently of inference requests.
func (k *CodexTurnStateKeeper) Configure(cfg *config.Config) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	k.cfg = cfg.CloneForRuntime()
	enabled := cfg != nil && cfg.Routing.CodexTurnStateEnabled != nil && *cfg.Routing.CodexTurnStateEnabled
	if !enabled {
		if k.cancel != nil {
			k.cancel()
		}
		k.ctx, k.cancel = nil, nil
		clear(k.entries)
		return
	}
	if k.ctx == nil {
		k.ctx, k.cancel = context.WithCancel(context.Background())
		ctx := k.ctx
		k.wg.Add(1)
		go func() {
			defer k.wg.Done()
			k.sweep(ctx)
			ticker := time.NewTicker(CodexTurnStateCheckInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					k.sweep(ctx)
				}
			}
		}()
	}
}

func (k *CodexTurnStateKeeper) Close() {
	k.mu.Lock()
	k.closed = true
	if k.cancel != nil {
		k.cancel()
	}
	k.ctx, k.cancel = nil, nil
	clear(k.entries)
	k.mu.Unlock()
	k.wg.Wait()
}

func codexTurnStateAccountID(auth *cliproxyauth.Auth) string {
	for _, key := range []string{"account_id", "chatgpt_account_id"} {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func codexTurnStateEligible(auth *cliproxyauth.Auth) bool {
	return auth != nil && auth.ID != "" && strings.EqualFold(auth.Provider, "codex") &&
		!auth.Disabled && !auth.AutoDisabled && auth.Status != cliproxyauth.StatusDisabled &&
		auth.AuthKind() == cliproxyauth.AuthKindOAuth && codexTurnStateAccountID(auth) != ""
}

func codexTurnStateBaseURL(auth *cliproxyauth.Auth) string {
	if base := strings.TrimSpace(auth.Attributes["base_url"]); base != "" {
		return strings.TrimRight(base, "/")
	}
	return codexTurnStateDefaultBaseURL
}

func codexTurnStateScope(auth *cliproxyauth.Auth) string {
	return fmt.Sprintf("%s\x00%s\x00%d", codexTurnStateAccountID(auth), codexTurnStateBaseURL(auth), codexTurnStateExpectedLength(auth))
}

func codexTurnStateExpectedLength(auth *cliproxyauth.Auth) int {
	plan := strings.TrimSpace(auth.Attributes["plan_type"])
	for _, key := range []string{"plan_type", "chatgpt_plan_type"} {
		if plan != "" {
			break
		}
		value, _ := auth.Metadata[key].(string)
		plan = strings.TrimSpace(value)
	}
	if plan == "" {
		for _, key := range []string{"access_token", "id_token"} {
			token, _ := auth.Metadata[key].(string)
			if token == "" {
				continue
			}
			claims, errParse := codexauth.ParseJWTToken(token)
			if errParse == nil {
				plan = strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
				if plan != "" {
					break
				}
			}
		}
	}
	plan = strings.ToLower(plan)
	if strings.HasPrefix(plan, "team") || strings.HasPrefix(plan, "business") || strings.HasPrefix(plan, "self_serve_business") {
		return codexTurnStateTeamLength
	}
	return codexTurnStatePersonalLength
}

func (k *CodexTurnStateKeeper) entryLocked(auth *cliproxyauth.Auth) *codexTurnStateEntry {
	scope := codexTurnStateScope(auth)
	entry := k.entries[auth.ID]
	if entry == nil || entry.scope != scope {
		if entry != nil && entry.cancel != nil {
			entry.cancel()
		}
		entry = &codexTurnStateEntry{scope: scope}
		k.entries[auth.ID] = entry
	}
	entry.auth = auth.Clone()
	return entry
}

func (k *CodexTurnStateKeeper) sweep(ctx context.Context) {
	auths := k.source()
	k.mu.Lock()
	defer k.mu.Unlock()
	if ctx.Err() != nil || k.ctx != ctx {
		return
	}
	active := make(map[string]bool, len(auths))
	now := time.Now()
	for _, auth := range auths {
		if !codexTurnStateEligible(auth) {
			continue
		}
		active[auth.ID] = true
		entry := k.entryLocked(auth)
		if entry.expires.Sub(now) < CodexTurnStateRefreshBefore || (k.autoDegradeEnabledLocked() && auth.IsDegraded()) {
			k.refreshLocked(entry)
		}
	}
	for id, entry := range k.entries {
		if !active[id] {
			if entry.cancel != nil {
				entry.cancel()
			}
			delete(k.entries, id)
		}
	}
}

// Token returns whether this account is managed even while its first state is being fetched.
func (k *CodexTurnStateKeeper) Token(auth *cliproxyauth.Auth) (string, bool) {
	if k == nil || !codexTurnStateEligible(auth) {
		return "", false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ctx == nil {
		return "", false
	}
	entry := k.entryLocked(auth)
	now := time.Now()
	if entry.expires.Sub(now) < CodexTurnStateRefreshBefore && now.Sub(entry.lastAttempt) >= CodexTurnStateCheckInterval {
		k.refreshLocked(entry)
	}
	if !now.Before(entry.expires) {
		return "", true
	}
	return entry.token, true
}

func (k *CodexTurnStateKeeper) refreshLocked(entry *codexTurnStateEntry) {
	if entry.refreshing {
		return
	}
	entry.refreshing = true
	entry.lastAttempt = time.Now()
	ctx, cancel := context.WithCancel(k.ctx)
	entry.cancel = cancel
	auth, cfg := entry.auth.Clone(), k.cfg
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		defer cancel()
		select {
		case k.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-k.slots }()
		token, errFetch := k.fetch(ctx, cfg, auth)
		var expires time.Time
		observedState := false
		if errFetch == nil {
			token = strings.TrimSpace(token)
			_, errTimestamp := codexTurnStateTokenExpiry(token, time.Now())
			observedState = errTimestamp == nil
			expires, errFetch = codexTurnStateExpiry(auth, token, time.Now())
		}
		k.mu.Lock()
		if ctx.Err() != nil || k.entries[auth.ID] != entry {
			k.mu.Unlock()
			return
		}
		entry.observedLength = len(token)
		degraded := errFetch != nil
		if errFetch != nil {
			entry.lastError = errFetch.Error()
			if observedState {
				entry.token, entry.expires = "", time.Time{}
			}
		} else {
			entry.token, entry.expires, entry.lastError = token, expires, ""
		}
		update := k.updateScheduling
		automatic := k.autoDegradeEnabledLocked() && observedState && update != nil
		k.mu.Unlock()
		var errUpdate error
		if automatic {
			errUpdate = update(ctx, auth, degraded)
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		if ctx.Err() != nil || k.entries[auth.ID] != entry {
			return
		}
		entry.refreshing = false
		entry.cancel = nil
		if errUpdate != nil {
			entry.lastError = fmt.Sprintf("automatic scheduling update failed: %v", errUpdate)
		}
	}()
}

// Observe renews on a fresh state outside the account's expected length class.
// A late rejection of an older state cannot invalidate a newer account state.
func (k *CodexTurnStateKeeper) Observe(auth *cliproxyauth.Auth, usedToken, receivedToken string) {
	if k == nil || !codexTurnStateEligible(auth) || !strings.HasPrefix(receivedToken, "gAAAAA") {
		return
	}
	if _, errTimestamp := codexTurnStateTokenExpiry(receivedToken, time.Now()); errTimestamp != nil || codexTurnStateMatchesLength(auth, receivedToken) {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	entry := k.entries[auth.ID]
	if k.ctx == nil || entry == nil || entry.scope != codexTurnStateScope(auth) {
		return
	}
	if entry.token != "" && usedToken != entry.token {
		return
	}
	entry.auth = auth.Clone()
	entry.token, entry.expires = "", time.Time{}
	k.refreshLocked(entry)
}

func (k *CodexTurnStateKeeper) Snapshot() []CodexTurnStateStatus {
	k.mu.Lock()
	defer k.mu.Unlock()
	result := make([]CodexTurnStateStatus, 0, len(k.entries))
	now := time.Now()
	for id, entry := range k.entries {
		status := CodexTurnStateStatus{AuthID: id, Name: entry.auth.FileName, Status: "empty", Refreshing: entry.refreshing, LastError: entry.lastError, ObservedLength: entry.observedLength}
		if status.Name == "" {
			status.Name = id
		}
		if now.Before(entry.expires) && entry.token != "" {
			expires := entry.expires
			status.ExpiresAt = &expires
			status.RemainingSeconds = int64(entry.expires.Sub(now) / time.Second)
			status.TokenLength = len(entry.token)
			status.Status = "active"
		} else if entry.lastError != "" {
			status.Status = "error"
		} else if entry.refreshing {
			status.Status = "refreshing"
		}
		result = append(result, status)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AuthID < result[j].AuthID })
	return result
}

func codexTurnStateDegraded(token string) bool {
	token = strings.TrimSpace(token)
	return strings.HasPrefix(token, "gAAAAA") && len(token) >= 308 && len(token) <= 316
}

func codexTurnStateExpiry(auth *cliproxyauth.Auth, token string, now time.Time) (time.Time, error) {
	if codexTurnStateDegraded(token) {
		return time.Time{}, fmt.Errorf("upstream returned a 312-length turn state")
	}
	expectedLength := codexTurnStateExpectedLength(auth)
	if !codexTurnStateMatchesLength(auth, token) {
		return time.Time{}, fmt.Errorf("upstream did not return a %d-length turn state for this account (received length %d)", expectedLength, len(token))
	}
	return codexTurnStateTokenExpiry(token, now)
}

func codexTurnStateMatchesLength(auth *cliproxyauth.Auth, token string) bool {
	expected := codexTurnStateExpectedLength(auth)
	return strings.HasPrefix(token, "gAAAAA") && len(token) >= expected-codexTurnStateLengthTolerance && len(token) <= expected+codexTurnStateLengthTolerance
}

func codexTurnStateTokenExpiry(token string, now time.Time) (time.Time, error) {
	if !strings.HasPrefix(token, "gAAAAA") {
		return time.Time{}, fmt.Errorf("invalid turn-state prefix")
	}
	raw, errDecode := base64.RawURLEncoding.DecodeString(strings.TrimRight(token, "="))
	if errDecode != nil || len(raw) < 9 || raw[0] != 0x80 {
		return time.Time{}, fmt.Errorf("invalid turn-state timestamp")
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(raw[1:9])), 0)
	expires := issued.Add(CodexTurnStateTTL)
	if issued.After(now) || !expires.After(now) {
		return time.Time{}, fmt.Errorf("turn state is expired or has a future timestamp")
	}
	return expires, nil
}

func fetchCodexTurnState(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (string, error) {
	model := codexTurnStateProbeModel
	if cfg != nil && strings.TrimSpace(cfg.Codex.TurnStateProbeModel) != "" {
		model = strings.TrimSpace(cfg.Codex.TurnStateProbeModel)
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"model": model, "store": false, "stream": true,
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "."}}}},
	})
	if errMarshal != nil {
		return "", errMarshal
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, codexTurnStateBaseURL(auth)+"/responses", bytes.NewReader(payload))
	if errRequest != nil {
		return "", fmt.Errorf("create turn-state probe: %w", errRequest)
	}
	token, _ := auth.Metadata["access_token"].(string)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-ID", codexTurnStateAccountID(auth))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	// Collection is explicitly direct, including when a process-wide proxy is configured.
	directAuth := auth.Clone()
	directAuth.ProxyURL = "direct"
	client := NewCodexHTTPClient(ctx, nil, directAuth, 0)
	response, errDo := client.Do(req)
	if errDo != nil {
		return "", fmt.Errorf("turn-state probe connection failed: %w", errDo)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.Debug("turn-state probe response close failed")
		}
	}()
	state := strings.TrimSpace(response.Header.Get(CodexTurnStateHeader))
	if state == "" {
		var failure struct {
			Detail string `json:"detail"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if response.StatusCode >= http.StatusBadRequest {
			_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&failure)
		}
		detail := failure.Detail
		if detail == "" {
			detail = failure.Error.Message
		}
		if token != "" {
			detail = strings.ReplaceAll(detail, token, "[REDACTED]")
		}
		if detail != "" {
			return "", fmt.Errorf("turn-state probe returned HTTP %d: %s", response.StatusCode, detail)
		}
		return "", fmt.Errorf("turn-state probe returned HTTP %d without %s", response.StatusCode, CodexTurnStateHeader)
	}
	return state, nil
}

type codexTurnStateTransport struct {
	base   http.RoundTripper
	keeper *CodexTurnStateKeeper
	auth   *cliproxyauth.Auth
}

func (k *CodexTurnStateKeeper) WrapHTTPClient(client *http.Client, auth *cliproxyauth.Auth) *http.Client {
	if k == nil {
		return client
	}
	cloned := *client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cloned.Transport = &codexTurnStateTransport{base: base, keeper: k, auth: auth}
	return &cloned
}

func (t *codexTurnStateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || !strings.Contains(req.URL.Path, "/responses") {
		return t.base.RoundTrip(req)
	}
	token, managed := t.keeper.Token(t.auth)
	if !managed {
		return t.base.RoundTrip(req)
	}
	outgoing := req.Clone(req.Context())
	clearCodexTurnStateHeader(outgoing.Header)
	if token != "" {
		outgoing.Header.Set(CodexTurnStateHeader, token)
	}
	if req.Body != nil {
		body, errRead := io.ReadAll(req.Body)
		if errClose := req.Body.Close(); errClose != nil {
			log.Debug("turn-state request body close failed")
		}
		if errRead != nil {
			return nil, errRead
		}
		body, errSet := sjson.DeleteBytes(body, codexTurnStateMetadataPath)
		if errSet != nil {
			return nil, errSet
		}
		outgoing.Body = io.NopCloser(bytes.NewReader(body))
		outgoing.ContentLength = int64(len(body))
		outgoing.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	response, errDo := t.base.RoundTrip(outgoing)
	if errDo == nil {
		t.keeper.Observe(t.auth, token, response.Header.Get(CodexTurnStateHeader))
	}
	return response, errDo
}

func clearCodexTurnStateHeader(headers http.Header) {
	for key := range headers {
		if strings.EqualFold(key, CodexTurnStateHeader) {
			delete(headers, key)
		}
	}
}

func (k *CodexTurnStateKeeper) StampWebsocket(auth *cliproxyauth.Auth, headers http.Header, payload []byte) ([]byte, string) {
	token, managed := k.Token(auth)
	if !managed {
		return payload, ""
	}
	clearCodexTurnStateHeader(headers)
	if token == "" {
		payload, _ = sjson.DeleteBytes(payload, codexTurnStateMetadataPath)
	} else {
		payload, _ = sjson.SetBytes(payload, codexTurnStateMetadataPath, token)
	}
	return payload, token
}

func (k *CodexTurnStateKeeper) ObserveWebsocket(auth *cliproxyauth.Auth, usedToken string, payload []byte) {
	if k == nil {
		return
	}
	for _, token := range codexTurnStateTokens(payload) {
		k.Observe(auth, usedToken, token)
	}
}

func codexTurnStateTokens(payload []byte) []string {
	if !bytes.Contains(bytes.ToLower(payload), []byte("turn-state")) && !bytes.Contains(payload, []byte("current_turn_state")) {
		return nil
	}
	var value any
	if json.Unmarshal(payload, &value) != nil {
		return nil
	}
	var tokens []string
	var visit func(any)
	visit = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			for key, child := range node {
				if strings.EqualFold(key, CodexTurnStateHeader) || key == "current_turn_state" {
					if token, ok := child.(string); ok {
						tokens = append(tokens, token)
					}
				} else {
					visit(child)
				}
			}
		case []any:
			for _, child := range node {
				visit(child)
			}
		}
	}
	visit(value)
	return tokens
}

func RedactCodexTurnStatePayload(payload []byte) []byte {
	for _, token := range codexTurnStateTokens(payload) {
		if strings.HasPrefix(token, "gAAAAA") {
			payload = bytes.ReplaceAll(payload, []byte(token), []byte("[REDACTED]"))
		}
	}
	return payload
}
