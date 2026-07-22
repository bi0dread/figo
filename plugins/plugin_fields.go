package plugins

import (
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
func (p *FieldsPlugin) FilterExpr(f figo.Figo, e figo.Expr) figo.Expr {
	p.mu.RLock()
	ignore := make(map[string]bool, len(p.ignoreFields))
	for k := range p.ignoreFields {
		ignore[k] = true
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
		ignored := make(map[string]bool, len(ignore)*2)
		for name := range ignore {
			ignored[name] = true
			ignored[fn(name)] = true
		}
		e = figo.PruneExprFields(e, func(field string) bool {
			return !ignored[field]
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

// FinalizeClauses implements ClauseFinalizer. The clause list itself passes
// through untouched (expression pruning happens in FilterExpr); the hook is
// where the SORT specification is enforced. sort= columns previously bypassed
// both the ignore list and the whitelist entirely, so a query could order —
// and, with page=take:1, probe value-by-value — a forbidden column the same
// plugin had just pruned from the WHERE clause.
func (p *FieldsPlugin) FinalizeClauses(f figo.Figo, clauses []figo.Expr) []figo.Expr {
	sort := f.GetSort()
	if sort == nil || len(sort.Columns) == 0 {
		return clauses
	}

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

	// Sort columns went through the naming strategy at parse time; match
	// registered names both verbatim and converted, exactly as FilterExpr does.
	fn := f.GetNamingFunc()
	ignored := make(map[string]bool, len(ignore)*2)
	for _, name := range ignore {
		ignored[name] = true
		ignored[fn(name)] = true
	}
	allowedConv := make(map[string]bool, len(allowed)*2)
	for _, name := range allowed {
		allowedConv[name] = true
		allowedConv[fn(name)] = true
	}

	kept := make([]figo.OrderByColumn, 0, len(sort.Columns))
	for _, col := range sort.Columns {
		if ignored[col.Name] {
			continue
		}
		if whitelist && !allowedConv[col.Name] {
			continue
		}
		kept = append(kept, col)
	}
	if len(kept) == len(sort.Columns) {
		return clauses
	}
	if len(kept) == 0 {
		f.SetSort(nil)
	} else {
		f.SetSort(&figo.OrderBy{Columns: kept})
	}
	return clauses
}
