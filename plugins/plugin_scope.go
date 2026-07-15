package plugins

import (
	figo "github.com/bi0dread/figo/v4"
)

import (
	"reflect"
	"sync"
)

// ScopePlugin guarantees that mandatory filters (scopes) are present in every
// built query — the classic use case is multi-tenant row scoping, where every
// query must carry tenant_id no matter what the caller asked for.
//
//	sp := plugins.NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: tenantID})
//	f.RegisterPlugin(sp)
//	f.AddFiltersFromString(untrustedDSL)
//	f.Build(figo.RawAdapter{})
//	// rendered WHERE always includes AND tenant_id = ?
//
// The scope is injected through the ClauseFinalizer hook, which runs at the
// end of EVERY Build — including a Build with no filters at all, so an
// unfiltered query cannot escape the scope. Injection happens after field
// pruning (FieldsPlugin), so a whitelist never strips the scope, and the
// scope survives rebuilds because Build re-runs finalizers each time.
type ScopePlugin struct {
	mu     sync.RWMutex
	scopes []figo.Expr
}

// NewScopePlugin creates a scope plugin enforcing the given filters
func NewScopePlugin(scopes ...figo.Expr) *ScopePlugin {
	p := &ScopePlugin{}
	p.AddScope(scopes...)
	return p
}

// Name implements Plugin
func (p *ScopePlugin) Name() string { return "figo-scope" }

// Version implements Plugin
func (p *ScopePlugin) Version() string { return "1.0.0" }

// Initialize implements Plugin
func (p *ScopePlugin) Initialize(figo.Figo) error { return nil }

// BeforeQuery implements Plugin
func (p *ScopePlugin) BeforeQuery(figo.Figo, any) error { return nil }

// AfterQuery implements Plugin
func (p *ScopePlugin) AfterQuery(figo.Figo, any, interface{}) error { return nil }

// BeforeParse implements Plugin
func (p *ScopePlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) { return dsl, nil }

// AfterParse implements Plugin
func (p *ScopePlugin) AfterParse(figo.Figo, string) error { return nil }

// AddScope registers additional mandatory filters
func (p *ScopePlugin) AddScope(scopes ...figo.Expr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range scopes {
		if s != nil {
			p.scopes = append(p.scopes, s)
		}
	}
}

// GetScopes returns a copy of the registered scopes
func (p *ScopePlugin) GetScopes() []figo.Expr {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]figo.Expr, len(p.scopes))
	copy(out, p.scopes)
	return out
}

// FinalizeClauses implements ClauseFinalizer: it appends each scope as a
// top-level clause (adapters AND all top-level clauses together). A scope
// already present is not appended again, so repeated Builds on an instance
// that keeps its clauses (empty-DSL rebuilds) don't accumulate duplicates.
func (p *ScopePlugin) FinalizeClauses(_ figo.Figo, clauses []figo.Expr) []figo.Expr {
	for _, s := range p.GetScopes() {
		if !containsEqualExpr(clauses, s) {
			// Clone so callers mutating the scope expression (or Walk
			// rewriting the tree) can't alias the plugin's copy.
			clauses = append(clauses, figo.CloneExpr(s))
		}
	}
	return clauses
}

// containsEqualExpr reports whether an expression deep-equal to e is already
// present in the clause list.
func containsEqualExpr(clauses []figo.Expr, e figo.Expr) bool {
	for _, c := range clauses {
		if reflect.DeepEqual(c, e) {
			return true
		}
	}
	return false
}
