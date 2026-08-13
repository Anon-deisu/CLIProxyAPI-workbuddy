package cursor

import (
	"sync"
	"time"
)

// The prompt ledger remembers, per conversation, how many prompt tokens the
// last request in that conversation carried.
//
// It exists for the requests Cursor never bills: a turn that pauses for a
// client tool ends the HTTP request before TurnEnded, so the relay has to
// estimate its usage. An estimate that calls the whole prompt fresh input is
// wrong in the way that matters most for cost dashboards — in an agent loop
// every request shares its prefix with the previous one, and Cursor's cache
// serves that prefix (the turns Cursor does bill show hit rates above 99%).
// Knowing the previous prompt size lets the estimate split prefix from new
// suffix the same way Anthropic bills an agent loop: prefix as cache read,
// new suffix as cache write.

const (
	promptLedgerTTL      = 6 * time.Hour
	promptLedgerMaxSize  = 8192
	promptLedgerMinTotal = 1
)

type promptLedgerEntry struct {
	total int64
	at    time.Time
}

var (
	promptLedgerMu sync.Mutex
	promptLedger   = map[string]promptLedgerEntry{}
)

// RememberPromptTotal records the latest prompt size for a conversation.
func RememberPromptTotal(conversationID string, total int64) {
	if conversationID == "" || total < promptLedgerMinTotal {
		return
	}
	promptLedgerMu.Lock()
	defer promptLedgerMu.Unlock()
	if len(promptLedger) >= promptLedgerMaxSize {
		prunePromptLedgerLocked()
	}
	promptLedger[conversationID] = promptLedgerEntry{total: total, at: time.Now()}
}

// LastPromptTotal returns the prompt size of the previous request in this
// conversation, if one is known and recent.
func LastPromptTotal(conversationID string) (int64, bool) {
	if conversationID == "" {
		return 0, false
	}
	promptLedgerMu.Lock()
	defer promptLedgerMu.Unlock()
	entry, ok := promptLedger[conversationID]
	if !ok {
		return 0, false
	}
	if time.Since(entry.at) > promptLedgerTTL {
		delete(promptLedger, conversationID)
		return 0, false
	}
	return entry.total, true
}

func prunePromptLedgerLocked() {
	cutoff := time.Now().Add(-promptLedgerTTL)
	for id, entry := range promptLedger {
		if entry.at.Before(cutoff) {
			delete(promptLedger, id)
		}
	}
	// Still full of live entries: start over rather than grow without bound.
	// A ledger miss only degrades one estimate's cache split.
	if len(promptLedger) >= promptLedgerMaxSize {
		promptLedger = map[string]promptLedgerEntry{}
	}
}
