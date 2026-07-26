package plugins

import (
	"strings"
	"sync"

	figo "github.com/bi0dread/figo/v4"
)

// Field policy (ignore fields and the allowed-fields whitelist) is provided as
// a plugin rather than core figo state. FieldsPlugin implements ExprFilter, so
// once registered its pruning applies wherever expressions enter the clause
// tree: Build (the parsed DSL and every preload condition) and AddFilter.
//
//	fp := plugins.NewFieldsPlugin()
//	fp.AddIgnoreFields("internal_flag")
//	fp.SetAllowedFields("id", "name", "email")
//	fp.EnableFieldWhitelist()
//	f.RegisterPlugin(fp)
//
// Select fields (AddSelectFields) remain on the figo.Figo instance — they are
// projection state consumed by the adapters at render time, not filter policy.
type FieldsPlugin struct {
	mu             sync.RWMutex
	ignoreFields   map[string]bool
	allowedFields  map[string]bool
	fieldWhitelist bool
}

// NewFieldsPlugin creates a new field-policy plugin
func NewFieldsPlugin() *FieldsPlugin {
	return &FieldsPlugin{
		ignoreFields:  make(map[string]bool),
		allowedFields: make(map[string]bool),
	}
}

// Name implements Plugin
func (p *FieldsPlugin) Name() string { return "figo-fields" }

// Version implements Plugin
func (p *FieldsPlugin) Version() string { return "1.0.0" }

// Initialize implements Plugin
func (p *FieldsPlugin) Initialize(figo.Figo) error { return nil }

// BeforeQuery implements Plugin
func (p *FieldsPlugin) BeforeQuery(figo.Figo, any) error { return nil }

// AfterQuery implements Plugin
func (p *FieldsPlugin) AfterQuery(figo.Figo, any, any) error { return nil }

// BeforeParse implements Plugin
func (p *FieldsPlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) { return dsl, nil }

// AfterParse implements Plugin
func (p *FieldsPlugin) AfterParse(figo.Figo, string) error { return nil }

// AddIgnoreFields registers fields whose conditions are pruned from every
// expression entering the clause tree
func (p *FieldsPlugin) AddIgnoreFields(fields ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, field := range fields {
		p.ignoreFields[field] = true
	}
}

// GetIgnoreFields returns a copy of the ignored-fields set
func (p *FieldsPlugin) GetIgnoreFields() map[string]bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make(map[string]bool, len(p.ignoreFields))
	for k, v := range p.ignoreFields {
		result[k] = v
	}
	return result
}

// SetAllowedFields sets the list of allowed fields for querying (replacing any
// previous list). Enforcement requires EnableFieldWhitelist.
func (p *FieldsPlugin) SetAllowedFields(fields ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allowedFields = make(map[string]bool, len(fields))
	for _, field := range fields {
		p.allowedFields[field] = true
	}
}

// GetAllowedFields returns a copy of the allowed-fields set
func (p *FieldsPlugin) GetAllowedFields() map[string]bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make(map[string]bool, len(p.allowedFields))
	for k, v := range p.allowedFields {
		result[k] = v
	}
	return result
}

// EnableFieldWhitelist enables field whitelist enforcement
func (p *FieldsPlugin) EnableFieldWhitelist() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fieldWhitelist = true
}

// DisableFieldWhitelist disables field whitelist enforcement
func (p *FieldsPlugin) DisableFieldWhitelist() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fieldWhitelist = false
}

// IsFieldWhitelistEnabled returns whether the field whitelist is enabled
func (p *FieldsPlugin) IsFieldWhitelistEnabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.fieldWhitelist
}

// IsFieldAllowed checks if a field is allowed for querying. With the whitelist
// disabled, every field is allowed.
func (p *FieldsPlugin) IsFieldAllowed(field string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.fieldWhitelist {
		return true
	}
	return p.allowedFields[field]
}

// FilterExpr implements ExprFilter: it prunes conditions on ignored fields,
// then (when the whitelist is enabled) conditions on disallowed fields.
// Expression fields have already been through the instance's naming strategy,
// so each registered ignore AND allowed name is matched both verbatim and in
// its converted form — callers may register either spelling. (The whitelist
// previously matched only the converted form, so SetAllowedFields("userName")
// under snake_case naming pruned the legitimate user_name filter.)
//
// The ignore list additionally matches case-insensitively and past a table
// qualifier (see newDenyMatcher): under NoChangeNaming the byte-exact lookup
// let `Password=` and `probe_users.password=` hit the very column the ignore
// list exists to hide.
func (p *FieldsPlugin) FilterExpr(f figo.Figo, e figo.Expr) figo.Expr {
	p.mu.RLock()
	ignore := make([]string, 0, len(p.ignoreFields))
	for k := range p.ignoreFields {
		ignore = append(ignore, k)
	}
	allowed := make(map[string]bool, len(p.allowedFields))
	for k := range p.allowedFields {
		allowed[k] = true
	}
	whitelist := p.fieldWhitelist
	p.mu.RUnlock()

	// FilterExpr runs outside f's lock, so reading naming state through the
	// public getter is safe.
	fn := f.GetNamingFunc() // never nil: SnakeCaseNaming is the default

	if len(ignore) > 0 {
		denied := newDenyMatcher(ignore, fn)
		e = figo.PruneExprFields(e, func(field string) bool {
			return !denied(field)
		})
	}

	if e != nil && whitelist {
		allowedConv := make(map[string]bool, len(allowed)*2)
		for name := range allowed {
			allowedConv[name] = true
			allowedConv[fn(name)] = true
		}
		e = figo.PruneExprFields(e, func(field string) bool {
			return allowedConv[field]
		})
	}
	return e
}

// canonicalPolicyName case-folds a field name. MySQL and SQLite resolve column
// identifiers case-INSENSITIVELY, so `Password` and `password` are one column
// to the engine while a byte-exact map lookup treats them as two.
func canonicalPolicyName(name string) string { return strings.ToLower(name) }

// terminalPolicyName case-folds and drops a table qualifier: every SQL dialect
// resolves `probe_users.password` and `password` to the same column.
func terminalPolicyName(name string) string {
	n := canonicalPolicyName(name)
	if i := strings.LastIndexByte(n, '.'); i >= 0 {
		return n[i+1:]
	}
	return n
}

// newDenyMatcher builds the predicate a DENY list (FieldsPlugin's ignore list,
// ValidationPlugin's rules) matches incoming field names with. A registered
// name matches verbatim, in its naming-converted form, and case-folded; the
// INCOMING field is additionally stripped of a table qualifier before the
// case-folded lookup, so `probe_users.password` is matched by a registered
// `password`. A registered name keeps its own qualifier, so registering
// `orders.secret` does not silently deny the unrelated `secret`.
//
// Only the deny side is broadened. Applying the same folding to the ALLOW side
// (the whitelist) would admit spellings the policy never approved, and the
// whitelist already fails closed on them. Without this, under NoChangeNaming
// (where the naming func collapses nothing) a case variant or a qualified
// spelling of an ignored/validated column reached the database unfiltered.
func newDenyMatcher(names []string, fn figo.NamingFunc) func(string) bool {
	if len(names) == 0 {
		return func(string) bool { return false }
	}
	exact := make(map[string]bool, len(names)*2)
	folded := make(map[string]bool, len(names)*2)
	for _, n := range names {
		exact[n] = true
		folded[canonicalPolicyName(n)] = true
		if fn != nil {
			c := fn(n)
			exact[c] = true
			folded[canonicalPolicyName(c)] = true
		}
	}
	return func(field string) bool {
		if exact[field] {
			return true
		}
		if fn != nil && exact[fn(field)] {
			return true
		}
		if folded[canonicalPolicyName(field)] || folded[terminalPolicyName(field)] {
			return true
		}
		if fn != nil {
			conv := fn(field)
			if folded[canonicalPolicyName(conv)] || folded[terminalPolicyName(conv)] {
				return true
			}
		}
		return false
	}
}

// FinalizeClauses implements ClauseFinalizer. The clause list itself passes
// through untouched (expression pruning happens in FilterExpr); the hook is
// where the SORT specification and the PROJECTION are enforced. sort= columns
// previously bypassed both the ignore list and the whitelist entirely, so a
// query could order — and, with page=take:1, probe value-by-value — a
// forbidden column the same plugin had just pruned from the WHERE clause; the
// select-field set could likewise still project a column the plugin refused to
// filter or order by.
func (p *FieldsPlugin) FinalizeClauses(f figo.Figo, clauses []figo.Expr) []figo.Expr {
	p.mu.RLock()
	ignore := make([]string, 0, len(p.ignoreFields))
	for k := range p.ignoreFields {
		ignore = append(ignore, k)
	}
	allowed := make([]string, 0, len(p.allowedFields))
	for k := range p.allowedFields {
		allowed = append(allowed, k)
	}
	whitelist := p.fieldWhitelist
	p.mu.RUnlock()

	if len(ignore) == 0 && !whitelist {
		return clauses
	}

	// Sort columns went through the naming strategy at parse time, select
	// fields did not (adapters convert them at render time), so both spellings
	// are matched — exactly as FilterExpr does for registered names.
	fn := f.GetNamingFunc()
	denied := newDenyMatcher(ignore, fn)
	allowedConv := make(map[string]bool, len(allowed)*2)
	for _, name := range allowed {
		allowedConv[name] = true
		allowedConv[fn(name)] = true
	}

	permitted := func(name string) bool {
		if denied(name) {
			return false
		}
		if whitelist && !allowedConv[name] && !allowedConv[fn(name)] {
			return false
		}
		return true
	}

	p.enforceSort(f, permitted)
	if !p.enforceSelectFields(f, permitted, allowed, whitelist) {
		// Every requested column is forbidden and there is no permitted
		// projection to substitute. Leaving the caller's set intact returned
		// exactly the column the policy promises can never be returned, and
		// clearing it would render SELECT * — wider still. Fail closed with
		// the canonical never-true clause (an empty OrExpr renders 1=0), the
		// same choice Build makes for a preload whose every condition was
		// pruned.
		return []figo.Expr{figo.OrExpr{}}
	}
	return clauses
}

// enforceSort drops forbidden columns from the sort specification.
func (p *FieldsPlugin) enforceSort(f figo.Figo, permitted func(string) bool) {
	sort := f.GetSort()
	if sort == nil || len(sort.Columns) == 0 {
		return
	}

	kept := make([]figo.OrderByColumn, 0, len(sort.Columns))
	for _, col := range sort.Columns {
		if permitted(col.Name) {
			kept = append(kept, col)
		}
	}
	if len(kept) == len(sort.Columns) {
		return
	}
	if len(kept) == 0 {
		f.SetSort(nil)
		return
	}
	f.SetSort(&figo.OrderBy{Columns: kept})
}

// enforceSelectFields drops forbidden columns from the projection. It reports
// whether a safe projection remains; false means the caller must fail the
// query closed (see FinalizeClauses).
//
// Pruning must never WIDEN the projection: an empty select set means "select
// every column", so removing the last permitted entry would expose more than
// the caller asked for, not less. When nothing survives, fall back to the
// PERMITTED part of the whitelist if one is configured — the raw whitelist was
// written straight through, which handed back a column the same plugin's
// ignore list refuses in WHERE and ORDER BY. Instances that never called
// AddSelectFields still render SELECT * — projection defaults are the
// application's call, not this plugin's.
func (p *FieldsPlugin) enforceSelectFields(f figo.Figo, permitted func(string) bool, allowed []string, whitelist bool) bool {
	selected := f.GetSelectFields()
	if len(selected) == 0 {
		return true
	}

	kept := make([]string, 0, len(selected))
	for name := range selected {
		if permitted(name) {
			kept = append(kept, name)
		}
	}
	if len(kept) == len(selected) {
		return true
	}
	if len(kept) == 0 {
		if whitelist {
			safe := make([]string, 0, len(allowed))
			for _, name := range allowed {
				if permitted(name) {
					safe = append(safe, name)
				}
			}
			if len(safe) > 0 {
				f.SetSelectFields(safe...)
				return true
			}
		}
		return false
	}
	f.SetSelectFields(kept...)
	return true
}
