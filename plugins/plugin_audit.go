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
//
// A "parse" entry records a parse ATTEMPT: another plugin's AfterParse hook
// (LimitsPlugin, ValidationPlugin) may still reject the same DSL, in which
// case AddFiltersFromString rolls it back and returns the error. All
// AfterParse hooks run regardless of each other's errors, so the attempt is
// recorded consistently whatever order the plugins were registered in.
//
// A "parse-attempt" entry records the DSL exactly as the CALLER sent it,
// before any BeforeParse hook has seen it. Without it, DSL rejected in
// BeforeParse (SyntaxPlugin's strict mode) left no trace at all — malformed
// and probing input was the one thing invisible to the trail — and with repair
// enabled the only recorded DSL was the REPAIRED string, never what arrived.
// BeforeParse hooks DO short-circuit on the first error, so register the
// AuditPlugin BEFORE any plugin that can reject or rewrite in BeforeParse.
type AuditPlugin struct {
	mu      sync.RWMutex
	logger  *slog.Logger
	history []AuditEntry
	max     int
}

// AuditEntry is one recorded parse or render event
type AuditEntry struct {
	Kind   string    // "parse-attempt", "parse" or "query"
	DSL    string    // the DSL: as received (Kind "parse-attempt"), as parsed (Kind "parse"), or the instance's current DSL (Kind "query")
	Result string    // the rendered SQL / query description (Kind "query")
	Args   []any     // the bound parameters, when the render was parameterized (Kind "query")
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

// BeforeParse records the DSL as received and passes it through unchanged.
// This is the only entry that survives a rejection or a rewrite by another
// plugin's BeforeParse hook (see the type comment).
func (p *AuditPlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) {
	p.record(AuditEntry{Kind: "parse-attempt", DSL: dsl, At: time.Now()})
	if p.logger != nil {
		p.logger.Info("figo: dsl received", "dsl", dsl)
	}
	return dsl, nil
}

// AfterParse records the parsed DSL
func (p *AuditPlugin) AfterParse(_ figo.Figo, dsl string) error {
	p.record(AuditEntry{Kind: "parse", DSL: dsl, At: time.Now()})
	if p.logger != nil {
		p.logger.Info("figo: dsl parsed", "dsl", dsl)
	}
	return nil
}

// AfterQuery records the rendered statement.
//
// On the parameterized path the SQL alone answers "what SHAPE ran?" but not
// "what did it run against?" — the tenant id and the searched term live in
// Args, so they are recorded alongside it. A Query type this plugin does not
// model is printed with its CONTENTS (%+v): the old "%T" default rendered
// Mongo and Elasticsearch entries as the bare string "adapters.MongoFindQuery",
// which is zero information. The instance's DSL is carried onto the entry so a
// rendered statement can be tied back to the parse it came from.
func (p *AuditPlugin) AfterQuery(f figo.Figo, ctx any, result interface{}) error {
	entry := AuditEntry{Kind: "query", Ctx: fmt.Sprintf("%v", ctx), At: time.Now()}
	if f != nil {
		entry.DSL = f.GetDSL()
	}
	switch r := result.(type) {
	case string:
		entry.Result = r
	case figo.SQLQuery:
		entry.Result = r.SQL
		if len(r.Args) > 0 {
			entry.Args = append([]any(nil), r.Args...)
		}
	default:
		entry.Result = fmt.Sprintf("%+v", r)
	}
	p.record(entry)
	if p.logger != nil {
		p.logger.Info("figo: query rendered", "result", entry.Result, "args", entry.Args, "ctx", entry.Ctx)
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
