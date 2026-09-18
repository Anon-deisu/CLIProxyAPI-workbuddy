package usage

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// DefaultServiceTier is retained for direct SDK and non-OpenAI usage callers.
const DefaultServiceTier = "default"

// AutoServiceTier is the OpenAI request semantics when service_tier is omitted.
// OpenAI HTTP handlers set it explicitly, without changing other providers'
// historical direct-SDK default.
const AutoServiceTier = "auto"

// Record contains the usage statistics captured for a single provider request.
type Record struct {
	Provider string
	// ExecutorType stores the concrete executor type that handled the request.
	ExecutorType    string
	Model           string
	Alias           string
	APIKey          string
	SessionID       string
	ParentSessionID string
	AuthID          string
	AuthIndex       string
	// AccessTokenSHA256 identifies the OAuth token version without exposing the token.
	AccessTokenSHA256 string
	AuthType          string
	Source            string
	// ReasoningEffort stores the translated upstream thinking level for request event logs.
	ReasoningEffort string
	// ServiceTier stores the client-requested service tier.
	ServiceTier string
	// RequestServiceTier is a deprecated input-only alias retained for existing
	// plugin callers. It is normalized into ServiceTier and never emitted.
	RequestServiceTier string
	// ResponseServiceTier stores the final tier reported by the upstream response.
	ResponseServiceTier string
	// Generate reports whether the client requested actual generation.
	// nil or true means generation is enabled; only an explicit false disables generation.
	// Use GenerateFlag to set the value and GenerateEnabled to read it with the default.
	Generate *bool
	// Stream reports whether the request was executed in streaming mode.
	Stream      bool
	RequestedAt time.Time
	Latency     time.Duration
	TTFT        time.Duration
	Failed      bool
	Fail        Failure
	Detail      Detail
	// ResponseHeaders stores a snapshot of upstream response headers for usage sinks.
	ResponseHeaders http.Header
}

// Failure holds HTTP failure metadata for an upstream request attempt.
type Failure struct {
	StatusCode int
	Body       string
}

// Detail holds the token usage breakdown.
type Detail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
	TokenBreakdown      TokenBreakdown
	ResponseServiceTier string
}

// ModelPrice describes the built-in GPT price in USD per one million tokens.
type ModelPrice struct {
	InputPerMillionUSD           float64
	CachedInputPerMillionUSD     float64
	CacheCreationPerMillionUSD   float64
	OutputPerMillionUSD          float64
	LongInputPerMillionUSD       float64
	LongCachedInputPerMillionUSD float64
	LongOutputPerMillionUSD      float64
	LongContextThreshold         int64
}

// AuthCostSnapshot contains token and estimated cost totals for the current
// local-calendar-day accounting cycle of one auth.
type AuthCostSnapshot struct {
	AuthID               string    `json:"auth_id"`
	CycleDate            string    `json:"cycle_date"`
	InputTokens          int64     `json:"input_tokens"`
	OutputTokens         int64     `json:"output_tokens"`
	EstimatedCostUSD     float64   `json:"estimated_cost_usd"`
	UnpricedInputTokens  int64     `json:"unpriced_input_tokens"`
	UnpricedOutputTokens int64     `json:"unpriced_output_tokens"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// BuiltinGPTModelPrices is the CPA price table used for local cost estimates.
// Values are USD per one million input/output tokens and are independent from
// upstream account billing or subscription quota rules.
var BuiltinGPTModelPrices = map[string]ModelPrice{
	"gpt-4o":       {InputPerMillionUSD: 2.50, OutputPerMillionUSD: 10.00},
	"gpt-4o-mini":  {InputPerMillionUSD: 0.15, OutputPerMillionUSD: 0.60},
	"gpt-4.1":      {InputPerMillionUSD: 2.00, OutputPerMillionUSD: 8.00},
	"gpt-4.1-mini": {InputPerMillionUSD: 0.40, OutputPerMillionUSD: 1.60},
	"gpt-4.1-nano": {InputPerMillionUSD: 0.10, OutputPerMillionUSD: 0.40},
	"gpt-5":        {InputPerMillionUSD: 1.25, OutputPerMillionUSD: 10.00},
	"gpt-5-mini":   {InputPerMillionUSD: 0.25, OutputPerMillionUSD: 2.00},
	"gpt-5-nano":   {InputPerMillionUSD: 0.05, OutputPerMillionUSD: 0.40},
	"o1":           {InputPerMillionUSD: 15.00, OutputPerMillionUSD: 60.00},
	"o1-mini":      {InputPerMillionUSD: 3.00, OutputPerMillionUSD: 12.00},
	"o3":           {InputPerMillionUSD: 2.00, OutputPerMillionUSD: 8.00},
	"o3-mini":      {InputPerMillionUSD: 1.10, OutputPerMillionUSD: 4.40},
	"o4-mini":      {InputPerMillionUSD: 1.10, OutputPerMillionUSD: 4.40},
}

// BuiltinCodexModelPrices follows Codex2API's standard pricing table in USD
// per one million tokens. Codex gpt-6 and gpt-5.6 intentionally have no cache
// creation price; gpt-6 also has no long-context multiplier.
var BuiltinCodexModelPrices = map[string]ModelPrice{
	"gpt-5.2":             {InputPerMillionUSD: 1.75, CachedInputPerMillionUSD: 0.175, CacheCreationPerMillionUSD: 1.75, OutputPerMillionUSD: 14.00},
	"gpt-5.3-codex":       {InputPerMillionUSD: 1.75, CachedInputPerMillionUSD: 0.175, CacheCreationPerMillionUSD: 1.75, OutputPerMillionUSD: 14.00},
	"gpt-5.3-codex-spark": {InputPerMillionUSD: 1.25, CachedInputPerMillionUSD: 0.125, CacheCreationPerMillionUSD: 1.25, OutputPerMillionUSD: 10.00},
	"gpt-5.4":             {InputPerMillionUSD: 2.50, CachedInputPerMillionUSD: 0.25, CacheCreationPerMillionUSD: 2.50, OutputPerMillionUSD: 15.00, LongInputPerMillionUSD: 5.00, LongCachedInputPerMillionUSD: 0.50, LongOutputPerMillionUSD: 22.50, LongContextThreshold: 272000},
	"gpt-5.4-mini":        {InputPerMillionUSD: 0.75, CachedInputPerMillionUSD: 0.075, CacheCreationPerMillionUSD: 0.75, OutputPerMillionUSD: 4.50},
	"gpt-5.4-nano":        {InputPerMillionUSD: 0.20, CachedInputPerMillionUSD: 0.02, CacheCreationPerMillionUSD: 0.20, OutputPerMillionUSD: 1.25},
	"gpt-5.4-pro":         {InputPerMillionUSD: 30.00, CacheCreationPerMillionUSD: 30.00, OutputPerMillionUSD: 180.00, LongInputPerMillionUSD: 60.00, LongOutputPerMillionUSD: 270.00, LongContextThreshold: 272000},
	"gpt-5.5":             {InputPerMillionUSD: 5.00, CachedInputPerMillionUSD: 0.50, CacheCreationPerMillionUSD: 5.00, OutputPerMillionUSD: 30.00, LongInputPerMillionUSD: 10.00, LongCachedInputPerMillionUSD: 1.00, LongOutputPerMillionUSD: 45.00, LongContextThreshold: 272000},
	"gpt-5.5-pro":         {InputPerMillionUSD: 30.00, CacheCreationPerMillionUSD: 30.00, OutputPerMillionUSD: 180.00, LongInputPerMillionUSD: 60.00, LongOutputPerMillionUSD: 270.00, LongContextThreshold: 272000},
	"gpt-5.6-sol":         {InputPerMillionUSD: 5.00, CachedInputPerMillionUSD: 0.50, OutputPerMillionUSD: 30.00, LongInputPerMillionUSD: 10.00, LongCachedInputPerMillionUSD: 1.00, LongOutputPerMillionUSD: 45.00, LongContextThreshold: 272000},
	"gpt-5.6-terra":       {InputPerMillionUSD: 2.00, CachedInputPerMillionUSD: 0.20, OutputPerMillionUSD: 12.00, LongInputPerMillionUSD: 4.00, LongCachedInputPerMillionUSD: 0.40, LongOutputPerMillionUSD: 18.00, LongContextThreshold: 272000},
	"gpt-5.6-luna":        {InputPerMillionUSD: 0.20, CachedInputPerMillionUSD: 0.02, OutputPerMillionUSD: 1.20, LongInputPerMillionUSD: 0.40, LongCachedInputPerMillionUSD: 0.04, LongOutputPerMillionUSD: 1.80, LongContextThreshold: 272000},
	"gpt-6-astra":         {InputPerMillionUSD: 10.00, CachedInputPerMillionUSD: 1.00, OutputPerMillionUSD: 50.00},
}

type authCostLedgerState struct {
	AuthCostSnapshot
}

type authCostLedger struct {
	mu      sync.Mutex
	entries map[string]*authCostLedgerState
}

func newAuthCostLedger() *authCostLedger {
	return &authCostLedger{entries: make(map[string]*authCostLedgerState)}
}

func costCycleDate(now time.Time) string {
	return now.In(time.Local).Format("2006-01-02")
}

func normalizeCostModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ""
	}
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		model = model[slash+1:]
	}
	if suffix := strings.IndexAny(model, "(:"); suffix >= 0 {
		model = model[:suffix]
	}
	return strings.TrimSpace(model)
}

func lookupModelPrice(table map[string]ModelPrice, model string) (ModelPrice, bool) {
	model = normalizeCostModel(model)
	if price, ok := table[model]; ok {
		return price, true
	}
	bestName := ""
	var bestPrice ModelPrice
	for name, price := range table {
		if strings.HasPrefix(model, name+"-") {
			if len(name) > len(bestName) {
				bestName = name
				bestPrice = price
			}
		}
	}
	if bestName != "" {
		return bestPrice, true
	}
	return ModelPrice{}, false
}

func modelPrice(provider, model string) (ModelPrice, bool) {
	if strings.EqualFold(strings.TrimSpace(provider), "codex") {
		normalized := normalizeCostModel(model)
		if strings.HasPrefix(normalized, "gpt-6") || strings.HasPrefix(normalized, "gpt6") {
			model = "gpt-6-astra"
		} else if strings.HasPrefix(normalized, "gpt-5.6") || strings.HasPrefix(normalized, "gpt5.6") {
			if !strings.Contains(normalized, "-sol") && !strings.Contains(normalized, "-terra") && !strings.Contains(normalized, "-luna") {
				model = "gpt-5.6-sol"
			}
		}
		return lookupModelPrice(BuiltinCodexModelPrices, model)
	}
	return lookupModelPrice(BuiltinGPTModelPrices, model)
}

func estimateRecordCost(record Record) (float64, int64, int64, int64, int64) {
	detail := EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	var uncachedInput, cacheRead, cacheWrite, output int64
	if detail.TokenBreakdown.Valid() && detail.TokenBreakdown.Quality != TokenAccountingQualityUnclassified && detail.TokenBreakdown.Input.TotalTokens > 0 {
		uncachedInput = detail.TokenBreakdown.Input.UncachedTokens
		cacheRead = detail.TokenBreakdown.Input.CacheReadTokens
		cacheWrite = detail.TokenBreakdown.Input.CacheWriteTokens
		output = detail.TokenBreakdown.Output.TotalTokens
	} else {
		uncachedInput = detail.InputTokens
		cacheRead = detail.CacheReadTokens
		cacheWrite = detail.CacheCreationTokens
		output = detail.OutputTokens
		if uncachedInput > cacheRead+cacheWrite {
			uncachedInput -= cacheRead + cacheWrite
		}
	}
	inputTotal := uncachedInput + cacheRead + cacheWrite
	price, ok := modelPrice(record.Provider, record.Model)
	if !ok {
		return 0, inputTotal, output, inputTotal, output
	}
	if price.LongContextThreshold > 0 && inputTotal >= price.LongContextThreshold && price.LongInputPerMillionUSD > 0 {
		price.InputPerMillionUSD = price.LongInputPerMillionUSD
		price.CachedInputPerMillionUSD = price.LongCachedInputPerMillionUSD
		price.OutputPerMillionUSD = price.LongOutputPerMillionUSD
	}
	cost := float64(uncachedInput)*price.InputPerMillionUSD/1_000_000 + float64(cacheRead)*price.CachedInputPerMillionUSD/1_000_000 + float64(cacheWrite)*price.CacheCreationPerMillionUSD/1_000_000 + float64(output)*price.OutputPerMillionUSD/1_000_000
	return cost, inputTotal, output, 0, 0
}

func (l *authCostLedger) stateLocked(authID string, now time.Time) *authCostLedgerState {
	if l.entries == nil {
		l.entries = make(map[string]*authCostLedgerState)
	}
	cycleDate := costCycleDate(now)
	state := l.entries[authID]
	if state == nil {
		state = &authCostLedgerState{AuthCostSnapshot: AuthCostSnapshot{AuthID: authID, CycleDate: cycleDate}}
		l.entries[authID] = state
	} else if state.CycleDate != cycleDate {
		state.AuthCostSnapshot = AuthCostSnapshot{AuthID: authID, CycleDate: cycleDate}
	}
	return state
}

func (l *authCostLedger) record(record Record, now time.Time) {
	if l == nil || strings.TrimSpace(record.AuthID) == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.stateLocked(strings.TrimSpace(record.AuthID), now)
	cost, inputTokens, outputTokens, unpricedInput, unpricedOutput := estimateRecordCost(record)
	state.InputTokens += inputTokens
	state.OutputTokens += outputTokens
	state.EstimatedCostUSD += cost
	state.UnpricedInputTokens += unpricedInput
	state.UnpricedOutputTokens += unpricedOutput
	state.UpdatedAt = now
}

func (l *authCostLedger) snapshot(authID string, now time.Time) AuthCostSnapshot {
	if l == nil || strings.TrimSpace(authID) == "" {
		return AuthCostSnapshot{AuthID: strings.TrimSpace(authID), CycleDate: costCycleDate(now)}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stateLocked(strings.TrimSpace(authID), now).AuthCostSnapshot
}

func (l *authCostLedger) reset(authID string, now time.Time) {
	if l == nil || strings.TrimSpace(authID) == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	trimmedID := strings.TrimSpace(authID)
	state := l.stateLocked(trimmedID, now)
	state.AuthCostSnapshot = AuthCostSnapshot{AuthID: trimmedID, CycleDate: costCycleDate(now), UpdatedAt: now}
}

type requestedModelAliasContextKey struct{}
type reasoningEffortContextKey struct{}
type serviceTierContextKey struct{}
type generateContextKey struct{}
type streamContextKey struct{}

// WithRequestedModelAlias stores the client-requested model name for usage sinks.
func WithRequestedModelAlias(ctx context.Context, alias string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return ctx
	}
	return context.WithValue(ctx, requestedModelAliasContextKey{}, alias)
}

// RequestedModelAliasFromContext returns the client-requested model name stored in ctx.
func RequestedModelAliasFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(requestedModelAliasContextKey{})
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

// WithReasoningEffort stores the client-requested reasoning effort for usage sinks.
func WithReasoningEffort(ctx context.Context, effort string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return ctx
	}
	return context.WithValue(ctx, reasoningEffortContextKey{}, effort)
}

// ReasoningEffortFromContext returns the client-requested reasoning effort stored in ctx.
func ReasoningEffortFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(reasoningEffortContextKey{})
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

// WithServiceTier stores the client-requested service tier for usage sinks.
func WithServiceTier(ctx context.Context, tier string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	tier = strings.TrimSpace(tier)
	if tier == "" {
		tier = DefaultServiceTier
	}
	return context.WithValue(ctx, serviceTierContextKey{}, tier)
}

// ServiceTierFromContext returns the client-requested service tier stored in ctx.
func ServiceTierFromContext(ctx context.Context) string {
	if ctx == nil {
		return DefaultServiceTier
	}
	raw := ctx.Value(serviceTierContextKey{})
	switch value := raw.(type) {
	case string:
		tier := strings.TrimSpace(value)
		if tier == "" {
			return DefaultServiceTier
		}
		return tier
	case []byte:
		tier := strings.TrimSpace(string(value))
		if tier == "" {
			return DefaultServiceTier
		}
		return tier
	default:
		return DefaultServiceTier
	}
}

// WithGenerate stores whether the client requested actual generation for usage sinks.
// Missing context values default to true; only an explicit false disables generation.
func WithGenerate(ctx context.Context, generate bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, generateContextKey{}, generate)
}

// GenerateFromContext returns whether the client requested actual generation.
// Missing values default to true.
func GenerateFromContext(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	raw := ctx.Value(generateContextKey{})
	switch value := raw.(type) {
	case bool:
		return value
	default:
		return true
	}
}

// WithStream stores whether the request was executed in streaming mode for usage sinks.
func WithStream(ctx context.Context, stream bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, streamContextKey{}, stream)
}

// StreamFromContext returns whether the request was executed in streaming mode.
// Missing values default to false.
func StreamFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw := ctx.Value(streamContextKey{})
	switch value := raw.(type) {
	case bool:
		return value
	default:
		return false
	}
}

// GenerateFlag returns a pointer suitable for Record.Generate.
func GenerateFlag(generate bool) *bool {
	return &generate
}

// GenerateEnabled reports whether generation is enabled for the record field.
// A nil value defaults to true so legacy callers that omit Generate keep the historical behavior.
func GenerateEnabled(generate *bool) bool {
	if generate == nil {
		return true
	}
	return *generate
}

// Plugin consumes usage records emitted by the proxy runtime.
type Plugin interface {
	HandleUsage(ctx context.Context, record Record)
}

type queueItem struct {
	ctx    context.Context
	record Record
}

// Manager maintains a queue of usage records and delivers them to registered plugins.
type Manager struct {
	once     sync.Once
	stopOnce sync.Once
	cancel   context.CancelFunc

	mu     sync.Mutex
	cond   *sync.Cond
	queue  []queueItem
	closed bool

	pluginsMu  sync.RWMutex
	plugins    []Plugin
	named      map[string]int
	costLedger *authCostLedger
}

// NewManager constructs a manager with a buffered queue.
func NewManager(buffer int) *Manager {
	m := &Manager{costLedger: newAuthCostLedger()}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// Start launches the background dispatcher. Calling Start multiple times is safe.
func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.once.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		var workerCtx context.Context
		workerCtx, m.cancel = context.WithCancel(ctx)
		go m.run(workerCtx)
	})
}

// Stop stops the dispatcher and drains the queue.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		m.cond.Broadcast()
	})
}

// Register appends a plugin to the delivery list.
func (m *Manager) Register(plugin Plugin) {
	if m == nil || plugin == nil {
		return
	}
	m.pluginsMu.Lock()
	m.plugins = append(m.plugins, plugin)
	m.pluginsMu.Unlock()
}

// RegisterNamed registers or replaces a plugin by name.
func (m *Manager) RegisterNamed(name string, plugin Plugin) {
	if m == nil || plugin == nil {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}

	m.pluginsMu.Lock()
	if m.named == nil {
		m.named = make(map[string]int)
	}
	if index, exists := m.named[name]; exists && index >= 0 && index < len(m.plugins) {
		m.plugins[index] = plugin
		m.pluginsMu.Unlock()
		return
	}
	m.named[name] = len(m.plugins)
	m.plugins = append(m.plugins, plugin)
	m.pluginsMu.Unlock()
}

// AuthCost returns the current local cost estimate for authID.
func (m *Manager) AuthCost(authID string) AuthCostSnapshot {
	if m == nil {
		return AuthCostSnapshot{AuthID: strings.TrimSpace(authID), CycleDate: costCycleDate(time.Now())}
	}
	if m.costLedger == nil {
		m.costLedger = newAuthCostLedger()
	}
	return m.costLedger.snapshot(authID, time.Now())
}

// ResetAuthCost clears the current cost cycle for authID.
func (m *Manager) ResetAuthCost(authID string) {
	if m == nil {
		return
	}
	if m.costLedger == nil {
		m.costLedger = newAuthCostLedger()
	}
	m.costLedger.reset(authID, time.Now())
}

// Publish enqueues a usage record for processing. If no plugin is registered
// the record will be discarded downstream.
func (m *Manager) Publish(ctx context.Context, record Record) {
	if m == nil {
		return
	}
	// ensure worker is running even if Start was not called explicitly
	m.Start(context.Background())
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.queue = append(m.queue, queueItem{ctx: ctx, record: record})
	m.mu.Unlock()
	m.cond.Signal()
}

func (m *Manager) run(ctx context.Context) {
	for {
		m.mu.Lock()
		for !m.closed && len(m.queue) == 0 {
			m.cond.Wait()
		}
		if len(m.queue) == 0 && m.closed {
			m.mu.Unlock()
			return
		}
		item := m.queue[0]
		m.queue = m.queue[1:]
		m.mu.Unlock()
		m.dispatch(item)
	}
}

func (m *Manager) dispatch(item queueItem) {
	if m.costLedger != nil {
		m.costLedger.record(item.record, time.Now())
	}
	m.pluginsMu.RLock()
	plugins := make([]Plugin, len(m.plugins))
	copy(plugins, m.plugins)
	m.pluginsMu.RUnlock()
	if len(plugins) == 0 {
		return
	}
	for _, plugin := range plugins {
		if plugin == nil {
			continue
		}
		safeInvoke(plugin, item.ctx, item.record)
	}
}

func safeInvoke(plugin Plugin, ctx context.Context, record Record) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("usage: plugin panic recovered: %v", r)
		}
	}()
	plugin.HandleUsage(ctx, record)
}

var defaultManager = NewManager(512)

// DefaultManager returns the global usage manager instance.
func DefaultManager() *Manager { return defaultManager }

// RegisterPlugin registers a plugin on the default manager.
func RegisterPlugin(plugin Plugin) { DefaultManager().Register(plugin) }

// RegisterNamedPlugin registers or replaces a named plugin on the default manager.
func RegisterNamedPlugin(name string, plugin Plugin) { DefaultManager().RegisterNamed(name, plugin) }

// AuthCost returns the current local cost estimate from the default usage manager.
func AuthCost(authID string) AuthCostSnapshot { return DefaultManager().AuthCost(authID) }

// ResetAuthCost clears the current cost cycle in the default usage manager.
func ResetAuthCost(authID string) { DefaultManager().ResetAuthCost(authID) }

// PublishRecord publishes a record using the default manager.
func PublishRecord(ctx context.Context, record Record) { DefaultManager().Publish(ctx, record) }

// StartDefault starts the default manager's dispatcher.
func StartDefault(ctx context.Context) { DefaultManager().Start(ctx) }

// StopDefault stops the default manager's dispatcher.
func StopDefault() { DefaultManager().Stop() }
