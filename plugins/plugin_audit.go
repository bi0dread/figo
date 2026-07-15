package plugins

import (
	figo "github.com/bi0dread/figo/v4"
)

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// AuditPlugin records every DSL the instance parses and every statement it
// renders — for compliance logging and "what did it actually run?" debugging.
//
//	ap := plugins.NewAuditPlugin(slog.Default(), 100)
//	f.RegisterPlugin(ap)
//	// ... parse and render ...
//	for _, e := range ap.History() { ... }
//
// Parsed DSL is captured via AfterParse, rendered output via AfterQuery (so
// on cached paths only real renders are audited, not cache hits). Entries go
// to the optional slog.Logger and into a bounded in-memory history (oldest
// entries are evicted first); pass historySize 0 to disable the history.
type AuditPlugin struct {
	mu      sync.RWMutex
	logger  *slog.Logger
	history []AuditEntry
	max     int
}

// AuditEntry is one recorded parse or render event
type AuditEntry struct {
	Kind   string    // "parse" or "query"
	DSL    string    // the parsed DSL (Kind "parse")
	Result string    // the rendered SQL / query description (Kind "query")
	Ctx    string    // render context (Kind "query")
	At     time.Time // when the event was recorded
}

// NewAuditPlugin creates an audit plugin. logger may be nil (history only);
// historySize bounds the in-memory history (0 disables it).
func NewAuditPlugin(logger *slog.Logger, historySize int) *AuditPlugin {
	if historySize < 0 {
		historySize = 0
	}
	return &AuditPlugin{logger: logger, max: historySize}
}

// Name implements Plugin
func (p *AuditPlugin) Name() string { return "figo-audit" }

// Version implements Plugin
func (p *AuditPlugin) Version() string { return "1.0.0" }

// Initialize implements Plugin
func (p *AuditPlugin) Initialize(figo.Figo) error { return nil }

// BeforeQuery implements Plugin
func (p *AuditPlugin) BeforeQuery(figo.Figo, any) error { return nil }

// BeforeParse implements Plugin
func (p *AuditPlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) { return dsl, nil }

// AfterParse records the parsed DSL
func (p *AuditPlugin) AfterParse(_ figo.Figo, dsl string) error {
	p.record(AuditEntry{Kind: "parse", DSL: dsl, At: time.Now()})
	if p.logger != nil {
		p.logger.Info("figo: dsl parsed", "dsl", dsl)
	}
	return nil
}

// AfterQuery records the rendered statement
func (p *AuditPlugin) AfterQuery(_ figo.Figo, ctx any, result interface{}) error {
	entry := AuditEntry{Kind: "query", Ctx: fmt.Sprintf("%v", ctx), At: time.Now()}
	switch r := result.(type) {
	case string:
		entry.Result = r
	case figo.SQLQuery:
		entry.Result = r.SQL
	default:
		entry.Result = fmt.Sprintf("%T", r)
	}
	p.record(entry)
	if p.logger != nil {
		p.logger.Info("figo: query rendered", "result", entry.Result, "ctx", entry.Ctx)
	}
	return nil
}

// record appends to the bounded history
func (p *AuditPlugin) record(e AuditEntry) {
	if p.max == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.history = append(p.history, e)
	if len(p.history) > p.max {
		p.history = p.history[len(p.history)-p.max:]
	}
}

// History returns a copy of the recorded entries, oldest first
func (p *AuditPlugin) History() []AuditEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]AuditEntry, len(p.history))
	copy(out, p.history)
	return out
}

// Clear empties the history
func (p *AuditPlugin) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.history = nil
}
