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
// Scopes apply to the TOP-LEVEL query only. A relation pulled in with
// load=[Orders:...] is fetched by a separate query on GORM (and is an
// unfiltered array on Mongo), so the parent's scope does not constrain the
// children — a child row belonging to another tenant comes back inside a
// correctly scoped parent. Which column scopes a child table is a property of
// that table, not of the parent, so it is not inferred: register it with
// AddPreloadScope (per relation) or AddPreloadScopeAll (every relation).
type ScopePlugin struct {
	mu            sync.RWMutex
	scopes        []figo.Expr
	preloadScopes map[string][]figo.Expr // relation -> mandatory conditions
	allPreloads   []figo.Expr            // applied to every preloaded relation
}

// NewScopePlugin creates a scope plugin enforcing the given filters
func NewScopePlugin(scopes ...figo.Expr) *ScopePlugin {
	p := &ScopePlugin{}
	p.AddScope(scopes...)
	return p
}

// AddPreloadScope registers mandatory conditions for ONE preloaded relation,
// named as it appears in load=[relation:...]. Use it when the child table
// carries its own tenant/soft-delete column:
//
//	sp := plugins.NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: t})
//	sp.AddPreloadScope("Orders", figo.EqExpr{Field: "tenant_id", Value: t})
func (p *ScopePlugin) AddPreloadScope(relation string, scopes ...figo.Expr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.preloadScopes == nil {
		p.preloadScopes = make(map[string][]figo.Expr)
	}
	for _, s := range scopes {
		if s != nil {
			p.preloadScopes[relation] = append(p.preloadScopes[relation], s)
		}
	}
}

// AddPreloadScopeAll registers mandatory conditions for EVERY preloaded
// relation. Only correct when every relation the DSL can name carries the
// column — an unknown column fails the preload query rather than widening it.
func (p *ScopePlugin) AddPreloadScopeAll(scopes ...figo.Expr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range scopes {
		if s != nil {
			p.allPreloads = append(p.allPreloads, s)
		}
	}
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
func (p *ScopePlugin) AfterQuery(figo.Figo, any, any) error { return nil }

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
func (p *ScopePlugin) FinalizeClauses(f figo.Figo, clauses []figo.Expr) []figo.Expr {
	for _, s := range p.GetScopes() {
		// Clone so callers mutating the scope expression (or Walk rewriting
		// the tree) can't alias the plugin's copy, and apply the instance's
		// naming func: the scope enters through this hook rather than through
		// AddFilter, so it was the one column in the statement that never got
		// converted — under a non-identity naming func an exclusion scope
		// addressed a column that did not exist and silently matched nothing.
		scoped := scopeExprFor(f, s)
		if !containsEqualExpr(clauses, scoped) {
			clauses = append(clauses, scoped)
		}
	}
	return clauses
}

// FinalizePreloads implements figo.PreloadFinalizer: it appends the mandatory
// conditions registered for this relation (plus any registered for every
// relation) to the preload's own conditions, which the adapters AND together.
func (p *ScopePlugin) FinalizePreloads(f figo.Figo, relation string, conds []figo.Expr) []figo.Expr {
	p.mu.RLock()
	relScopes := append([]figo.Expr(nil), p.preloadScopes[relation]...)
	allScopes := append([]figo.Expr(nil), p.allPreloads...)
	p.mu.RUnlock()

	for _, s := range append(relScopes, allScopes...) {
		scoped := scopeExprFor(f, s)
		if !containsEqualExpr(conds, scoped) {
			conds = append(conds, scoped)
		}
	}
	return conds
}

// scopeExprFor returns an independent copy of s with the instance's naming
// func applied to its field names.
func scopeExprFor(f figo.Figo, s figo.Expr) figo.Expr {
	c := figo.CloneExpr(s)
	if f == nil {
		return c
	}
	return figo.NormalizeExprFields(c, f.GetNamingFunc())
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
