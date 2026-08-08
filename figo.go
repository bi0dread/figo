package figo

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// Global configuration
var (
	// regexSQLOperator controls the SQL operator used for RegexExpr in SQL adapters.
	// Defaults to MySQL-compatible REGEXP. For Postgres, set to "~" or "~*".
	// Atomic: it may be reconfigured while adapters render on other goroutines.
	regexSQLOperator atomic.Value
)

func init() {
	regexSQLOperator.Store("REGEXP")
}

// SetRegexSQLOperator sets the SQL operator used to render regex in SQL adapters (Raw/GORM)
func SetRegexSQLOperator(op string) {
	op = strings.TrimSpace(op)
	if op == "" {
		return
	}
	regexSQLOperator.Store(op)
}

// GetRegexSQLOperator returns the configured SQL regex operator
func GetRegexSQLOperator() string { return regexSQLOperator.Load().(string) }

// NamingFunc transforms a DSL field name into the column/field name used by the
// target store. Register one with SetNamingFunc to supply custom naming logic
// (e.g. camelCase, a fixed prefix, or a lookup table), or use one of the
// built-ins: SnakeCaseNaming (the default) or NoChangeNaming.
type NamingFunc func(field string) string

// SnakeCaseNaming is the default NamingFunc: userName -> user_name.
var SnakeCaseNaming NamingFunc = func(field string) string {
	// snakeCaseWords treats leading underscores as separators and drops them
	// ("_id" -> "id"), which would silently break fields like Mongo's
	// canonical _id — split them off and restore them after conversion.
	trimmed := strings.TrimLeft(field, "_")
	prefix := field[:len(field)-len(trimmed)]
	// If conversion yields an empty string (a name of only separator runes),
	// fall back to the original name rather than silently dropping the column.
	result := snakeCaseWords(trimmed)
	if result == "" {
		return field
	}
	return prefix + result
}

// snakeCaseWords converts a name to snake_case: words split at separator
// runes ('_', '-', '.', ' ') and at a lower→UPPER case boundary, then join
// with '_' lowercased. It replicates the exact conversion previously
// delegated to github.com/gobeam/stringy (figo's only use of the module):
// acronym runs stay glued ("userID" -> "user_id", "HTTPServer" ->
// "httpserver"), digits never open a boundary ("myURL2Path" ->
// "my_url2path"), separator runs collapse ("user__name" -> "user_name"),
// and non-separator symbols pass through ("price$" -> "price$").
func snakeCaseWords(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	prevLower := false
	wordStarted := false
	pendingSep := false
	for _, r := range s {
		if r == '_' || r == '-' || r == '.' || r == ' ' {
			pendingSep = wordStarted // collapse runs; leading separators drop
			prevLower = false
			continue
		}
		if wordStarted && (pendingSep || (prevLower && unicode.IsUpper(r))) {
			b.WriteByte('_')
		}
		pendingSep = false
		wordStarted = true
		prevLower = unicode.IsLower(r)
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// NoChangeNaming leaves field names exactly as written in the DSL.
var NoChangeNaming NamingFunc = func(field string) string { return field }

// Operation names a DSL operator or directive token. The comparison values
// (OperationEq, OperationGt, ...) mirror the literal DSL spelling; the
// bracketed ones (OperationBetween, OperationIn, ...) are the DSL's
// angle-bracket operators; Sort/Page/Load are the query directives.
type Operation string

const (
	OperationEq       Operation = "="
	OperationGt       Operation = ">"
	OperationGte      Operation = ">="
	OperationLt       Operation = "<"
	OperationLte      Operation = "<="
	OperationNeq      Operation = "!="
	OperationNot      Operation = "not"
	OperationLike     Operation = "=^"
	OperationNotLike  Operation = "!=^"
	OperationRegex    Operation = "=~"
	OperationNotRegex Operation = "!=~"
	OperationAnd      Operation = "and"
	OperationOr       Operation = "or"
	OperationBetween  Operation = "<bet>"
	OperationIn       Operation = "<in>"
	OperationNotIn    Operation = "<nin>"
	OperationSort     Operation = "sort"
	OperationLoad     Operation = "load"
	OperationPage     Operation = "page"
	OperationChild    Operation = "----"
	OperationILike    Operation = ".=^"
	OperationIsNull   Operation = "<null>"
	OperationNotNull  Operation = "<notnull>"
)

// operationDirective marks a parser node standing in for a consumed
// sort=/page=/load= directive. It is internal (never produced by parseToken,
// never rendered) and exists so the precedence pass can see WHERE a directive
// sat in the token stream: a directive is not an operand, so the connector
// written next to it ("a=1 and sort=id:asc") joins nothing. Without the marker
// that connector reached reduceBinary unpaired and BuildE reported a
// "dangling connector" for input the built query expressed in full — including
// figo's own canonical examples — and a "not" written before a bare directive
// jumped onto the FOLLOWING predicate and inverted it.
const operationDirective Operation = "\x00directive"

// AdapterType removed: adapters are selected via Adapter objects

// Page is the pagination state: Skip rows to offset, Take rows to return.
// Take <= 0 means "no limit" on every adapter (GORM never renders LIMIT 0).
type Page struct {
	Skip int
	Take int
}

// Plugin System

// Plugin interface for extending Figo functionality
type Plugin interface {
	Name() string
	Version() string
	Initialize(f Figo) error
	BeforeQuery(f Figo, ctx any) error
	AfterQuery(f Figo, ctx any, result any) error
	BeforeParse(f Figo, dsl string) (string, error)
	AfterParse(f Figo, dsl string) error
}

// ExprFilter is an optional interface a Plugin may implement to transform or
// prune expressions as they enter the clause tree. Build applies it to the
// parsed expression (and every preload expression); AddFilter applies it to
// the programmatic expression. Return nil to drop the expression entirely.
//
// FilterExpr runs outside the instance's lock, so it may safely call back
// into the Figo's read methods (GetNamingFunc, GetClauses, ...).
type ExprFilter interface {
	FilterExpr(f Figo, e Expr) Expr
}

// ClauseFinalizer is an optional interface a Plugin may implement to
// transform the finished top-level clause list at the end of every Build —
// including a Build whose DSL produced no filters at all (the list may be
// empty), which is what lets a plugin GUARANTEE a clause is present (e.g.
// ScopePlugin's mandatory tenant filter). The returned slice replaces the
// instance's clauses.
//
// FinalizeClauses runs once per Build, after all ExprFilters, outside the
// instance's lock (calling back into read methods is safe).
type ClauseFinalizer interface {
	FinalizeClauses(f Figo, clauses []Expr) []Expr
}

// PreloadFinalizer is an optional interface a Plugin may implement to
// transform each preloaded relation's condition list at the end of every
// Build, the way ClauseFinalizer transforms the top-level clauses.
//
// It exists because the two policy hooks were asymmetric: ExprFilter already
// ran over preload conditions (so FieldsPlugin's pruning reached them), but
// nothing could ADD to them, so a mandatory filter was structurally confined
// to the top level. On GORM a preload is issued as a SEPARATE query, so a
// ScopePlugin tenant filter guarded the parent rows and not the children.
//
// FinalizePreloads runs once per Build, after all ExprFilters and before
// FinalizeClauses, outside the instance's lock. relation is the name as it
// appeared in load=[relation:...]; returning an empty list drops the preload —
// EXCEPT for a relation that was already unconditioned ("load=[Orders:]"),
// where an empty result is a no-op and the relation is still preloaded. An
// unconditioned preload has no conditions to remove, so "empty" cannot mean
// "the finalizer removed them all" there, and treating it that way made merely
// registering a plugin delete the preload.
type PreloadFinalizer interface {
	FinalizePreloads(f Figo, relation string, conditions []Expr) []Expr
}

// PluginManager manages plugins. Hooks run in REGISTRATION ORDER — the order
// is part of the contract (iterating the map made every dispatch order random,
// so two DSL-rewriting plugins composed differently from call to call).
type PluginManager struct {
	plugins map[string]Plugin
	order   []string // registration order; drives ListPlugins and every Execute*
	mu      sync.RWMutex
}

// NewPluginManager creates a new plugin manager
func NewPluginManager() *PluginManager {
	return &PluginManager{
		plugins: make(map[string]Plugin),
	}
}

// RegisterPlugin registers a plugin
func (pm *PluginManager) RegisterPlugin(plugin Plugin) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if plugin == nil {
		return fmt.Errorf("plugin cannot be nil")
	}

	name := plugin.Name()
	if name == "" {
		return fmt.Errorf("plugin name cannot be empty")
	}

	if _, exists := pm.plugins[name]; exists {
		return fmt.Errorf("plugin %s already registered", name)
	}

	pm.plugins[name] = plugin
	pm.order = append(pm.order, name)
	return nil
}

// UnregisterPlugin removes a plugin
func (pm *PluginManager) UnregisterPlugin(name string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if _, exists := pm.plugins[name]; !exists {
		return fmt.Errorf("plugin %s not found", name)
	}

	delete(pm.plugins, name)
	for i, n := range pm.order {
		if n == name {
			pm.order = append(pm.order[:i], pm.order[i+1:]...)
			break
		}
	}
	return nil
}

// GetPlugin retrieves a plugin by name
func (pm *PluginManager) GetPlugin(name string) (Plugin, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	plugin, exists := pm.plugins[name]
	return plugin, exists
}

// ListPlugins returns all registered plugins in registration order (the order
// every hook dispatch uses).
func (pm *PluginManager) ListPlugins() []Plugin {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	plugins := make([]Plugin, 0, len(pm.order))
	for _, name := range pm.order {
		plugins = append(plugins, pm.plugins[name])
	}
	return plugins
}

// Hooks run on a snapshot taken under the lock, with the lock released while
// user plugin code executes — a hook that calls back into the manager
// (Register/Unregister/List) must not deadlock.

// hasPlugins reports whether any plugin is registered. Build uses it to treat
// an EMPTY manager exactly like no manager at all: GetPluginManager constructs
// one lazily (so the documented ListPlugins/GetPlugin calls no longer panic on a
// fresh instance), and without this a read-only-looking getter changed what the
// next Build produced.
func (pm *PluginManager) hasPlugins() bool {
	if pm == nil {
		return false
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.order) > 0
}

// ExecuteBeforeQuery executes all plugins' BeforeQuery hooks
func (pm *PluginManager) ExecuteBeforeQuery(f Figo, ctx any) error {
	for _, plugin := range pm.ListPlugins() {
		if err := plugin.BeforeQuery(f, ctx); err != nil {
			return fmt.Errorf("plugin %s BeforeQuery error: %w", plugin.Name(), err)
		}
	}
	return nil
}

// ExecuteAfterQuery executes all plugins' AfterQuery hooks
func (pm *PluginManager) ExecuteAfterQuery(f Figo, ctx any, result any) error {
	for _, plugin := range pm.ListPlugins() {
		if err := plugin.AfterQuery(f, ctx, result); err != nil {
			return fmt.Errorf("plugin %s AfterQuery error: %w", plugin.Name(), err)
		}
	}
	return nil
}

// ExecuteBeforeParse executes all plugins' BeforeParse hooks. Like
// ExecuteAfterParse, every hook runs even when an earlier one errors: stopping
// at the first error made observer plugins registration-order-dependent — an
// AuditPlugin registered after a rejecting SyntaxPlugin never saw the rejected
// DSL at all, so probing input was the one thing invisible to the audit trail.
//
// BeforeParse also TRANSFORMS the string, so this is not quite AfterParse's
// shape: once a hook errors its output is discarded and the remaining hooks are
// fed the last good DSL, which keeps them observing what the caller actually
// sent rather than a half-applied rewrite. The first error is still the one
// returned, and an error still rejects the DSL.
func (pm *PluginManager) ExecuteBeforeParse(f Figo, dsl string) (string, error) {
	modifiedDSL := dsl
	var firstErr error
	for _, plugin := range pm.ListPlugins() {
		out, err := plugin.BeforeParse(f, modifiedDSL)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("plugin %s BeforeParse error: %w", plugin.Name(), err)
			}
			continue
		}
		if firstErr == nil {
			modifiedDSL = out
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	return modifiedDSL, nil
}

// ExecuteAfterParse executes all plugins' AfterParse hooks. Every hook runs
// even when an earlier one errors (errors are joined): short-circuiting made
// observer plugins registration-order-dependent — an AuditPlugin registered
// after a rejecting LimitsPlugin never saw the rejected DSL at all, while one
// registered before it recorded the same DSL as if it had been accepted.
func (pm *PluginManager) ExecuteAfterParse(f Figo, dsl string) error {
	var errs []error
	for _, plugin := range pm.ListPlugins() {
		if err := plugin.AfterParse(f, dsl); err != nil {
			errs = append(errs, fmt.Errorf("plugin %s AfterParse error: %w", plugin.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// ExecuteExprFilters runs every registered plugin that implements ExprFilter
// over the expression. A filter returning nil drops the expression and
// short-circuits the remaining filters.
func (pm *PluginManager) ExecuteExprFilters(f Figo, e Expr) Expr {
	for _, plugin := range pm.ListPlugins() {
		if e == nil {
			return nil
		}
		if filter, ok := plugin.(ExprFilter); ok {
			e = filter.FilterExpr(f, e)
		}
	}
	return e
}

// ExecutePreloadFinalizers runs every registered plugin that implements
// PreloadFinalizer over one relation's condition list.
func (pm *PluginManager) ExecutePreloadFinalizers(f Figo, relation string, conds []Expr) []Expr {
	for _, plugin := range pm.ListPlugins() {
		if fin, ok := plugin.(PreloadFinalizer); ok {
			conds = fin.FinalizePreloads(f, relation, conds)
		}
	}
	return conds
}

// ExecuteClauseFinalizers runs every registered plugin that implements
// ClauseFinalizer over the top-level clause list.
func (pm *PluginManager) ExecuteClauseFinalizers(f Figo, clauses []Expr) []Expr {
	for _, plugin := range pm.ListPlugins() {
		if fin, ok := plugin.(ClauseFinalizer); ok {
			clauses = fin.FinalizeClauses(f, clauses)
		}
	}
	return clauses
}

// ParseError represents a DSL parsing error with context
type ParseError struct {
	Message    string
	Position   int
	Line       int
	Column     int
	Context    string
	Suggestion string
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("Parse error at line %d, column %d: %s", e.Line, e.Column, e.Message)
	}
	return fmt.Sprintf("Parse error at position %d: %s", e.Position, e.Message)
}

// Expr represents an ORM-agnostic expression node
type Expr interface{ isExpr() }

// EqExpr matches rows whose Field equals Value (DSL: field=value).
type EqExpr struct {
	Field string
	Value any
}

// GteExpr matches rows whose Field is >= Value (DSL: field>=value).
type GteExpr struct {
	Field string
	Value any
}

// GtExpr matches rows whose Field is > Value (DSL: field>value).
type GtExpr struct {
	Field string
	Value any
}

// LtExpr matches rows whose Field is < Value (DSL: field<value).
type LtExpr struct {
	Field string
	Value any
}

// LteExpr matches rows whose Field is <= Value (DSL: field<=value).
type LteExpr struct {
	Field string
	Value any
}

// NeqExpr matches rows whose Field differs from Value (DSL: field!=value).
type NeqExpr struct {
	Field string
	Value any
}

// LikeExpr matches Field against a SQL LIKE pattern (DSL: field=^pattern).
// Mongo renders it as an anchored regex, Elasticsearch as a wildcard query.
type LikeExpr struct {
	Field string
	Value any
}

// RegexExpr matches Field against a regular expression (DSL: field=~pattern),
// with unanchored "contains" semantics on every adapter.
type RegexExpr struct {
	Field string
	Value any
}

// AndExpr matches when every operand matches.
type AndExpr struct{ Operands []Expr }

// OrExpr matches when at least one operand matches. An OrExpr with no
// renderable operands is a false disjunction: every adapter renders it as
// match-nothing (it never silently vanishes from the query).
type OrExpr struct{ Operands []Expr }

// NotExpr matches when NONE of its operands match: NOT(a OR b). All adapters
// implement this reading (SQL "NOT (a OR b)", Mongo $nor, GORM clause.Not,
// Elasticsearch must_not). The DSL parser only emits single-operand NotExpr;
// multi-operand forms come from AddFilter.
type NotExpr struct{ Operands []Expr }

// OrderByColumn is one sort key: a column name and its direction.
type OrderByColumn struct {
	Name string
	Desc bool
}

// OrderBy is the parsed sort= directive: sort keys in priority order.
type OrderBy struct{ Columns []OrderByColumn }

// Query is a marker interface for adapter-agnostic rendered queries
// Concrete types are provided per adapter (e.g., SQLQuery, MongoFindQuery)
// Query is the marker interface for adapter-agnostic rendered queries.
// IsQuery is exported so adapter packages (including third-party ones) can
// implement their own typed results.
type Query interface{ IsQuery() }

// SQLQuery represents a parametrized SQL statement
type SQLQuery struct {
	SQL  string
	Args []any
}

func (SQLQuery) IsQuery() {}

func (EqExpr) isExpr()    {}
func (GteExpr) isExpr()   {}
func (GtExpr) isExpr()    {}
func (LtExpr) isExpr()    {}
func (LteExpr) isExpr()   {}
func (NeqExpr) isExpr()   {}
func (LikeExpr) isExpr()  {}
func (RegexExpr) isExpr() {}
func (AndExpr) isExpr()   {}
func (OrExpr) isExpr()    {}
func (NotExpr) isExpr()   {}
func (OrderBy) isExpr()   {}

// InExpr matches rows whose Field equals any of Values (DSL: field<in>[...]).
// An empty Values list matches nothing on every adapter (never dropped).
type InExpr struct {
	Field  string
	Values []any
}

// NotInExpr matches rows whose Field equals none of Values (DSL:
// field<nin>[...]). An empty Values list matches everything.
type NotInExpr struct {
	Field  string
	Values []any
}

// BetweenExpr matches rows whose Field lies in [Low, High], inclusive on both
// ends on every adapter (DSL: field<bet>(low..high)).
type BetweenExpr struct {
	Field string
	Low   any
	High  any
}

// IsNullExpr matches rows whose Field is NULL (DSL: field<null> or field=null).
// Document stores (Mongo/Elasticsearch) also match documents missing the field.
type IsNullExpr struct{ Field string }

// NotNullExpr matches rows whose Field is not NULL (DSL: field<notnull> or
// field!=null).
type NotNullExpr struct{ Field string }

// ILikeExpr is a case-insensitive LikeExpr (DSL: field.=^pattern). SQL
// adapters render LOWER(col) LIKE LOWER(?), Mongo an (?i) regex.
type ILikeExpr struct {
	Field string
	Value any
}

func (InExpr) isExpr()      {}
func (NotInExpr) isExpr()   {}
func (BetweenExpr) isExpr() {}
func (IsNullExpr) isExpr()  {}
func (NotNullExpr) isExpr() {}
func (ILikeExpr) isExpr()   {}

// Advanced Operators for Phase 3

// JsonPathExpr represents JSON path operations
type JsonPathExpr struct {
	Field string
	Path  string
	Value any
	Op    string // "=", "!=", ">", "<", ">=", "<=", "contains", "exists"
}

// ArrayContainsExpr represents array contains operations
type ArrayContainsExpr struct {
	Field  string
	Values []any
}

// ArrayOverlapsExpr represents array overlap operations
type ArrayOverlapsExpr struct {
	Field  string
	Values []any
}

// FullTextSearchExpr represents full-text search operations
type FullTextSearchExpr struct {
	Field    string
	Query    string
	Language string // Optional language for full-text search
}

// GeoDistanceExpr represents geographical distance operations
type GeoDistanceExpr struct {
	Field     string
	Latitude  float64
	Longitude float64
	Distance  float64 // in kilometers
	Unit      string  // "km", "miles", "meters"
}

// CustomExpr represents custom operations. The SQL adapters (raw and GORM)
// invoke Handler with the Field exactly as held here (unquoted, unvalidated),
// the Operator and the Value; it returns a SQL fragment with '?' placeholders
// plus its bind args. The handler owns identifier QUOTING. The MongoDB and
// Elasticsearch adapters reject CustomExpr — its output is a SQL fragment.
//
// NAMING is not the handler's: AddFilter converts Field through the instance's
// NamingFunc (per '.'-separated segment) before storing it, exactly as it does
// for every other expression type — one statement must not contain two
// spellings of the same identifier. So pass the ORIGINAL spelling
// (CustomExpr{Field: "userName"} reaches a handler as "user_name" under the
// default SnakeCaseNaming), and a handler that dispatches on the field name
// should match the CONVERTED form. SetNamingFunc(NoChangeNaming) opts out.
// Expressions handed to the adapters by other routes (Walk, a plugin's
// FilterExpr) are rendered verbatim, unconverted.
type CustomExpr struct {
	Field    string
	Operator string
	Value    any
	Handler  func(field, operator string, value any) (string, []any, error)
}

// Implement Expr interface for new operators
func (JsonPathExpr) isExpr()       {}
func (ArrayContainsExpr) isExpr()  {}
func (ArrayOverlapsExpr) isExpr()  {}
func (FullTextSearchExpr) isExpr() {}
func (GeoDistanceExpr) isExpr()    {}
func (CustomExpr) isExpr()         {}

// Figo is the query-building facade: feed it DSL (AddFiltersFromString) or
// programmatic expressions (AddFilter), Build against an Adapter, then render
// via GetSqlString/GetQuery or the adapter package's Build* helpers. Create
// instances with New. All methods are safe for concurrent use; plugin hooks
// and Walk visitors run outside the internal lock.
type Figo interface {
	AddFiltersFromString(input string) error
	AddFilter(exp Expr)
	AddSelectFields(fields ...string)
	SetSelectFields(fields ...string)
	GetDSL() string
	SetPluginManager(manager *PluginManager)
	GetPluginManager() *PluginManager
	RegisterPlugin(plugin Plugin) error
	UnregisterPlugin(name string) error
	SetNamingFunc(fn NamingFunc)
	GetNamingFunc() NamingFunc
	SetPage(skip, take int)
	SetPageString(v string)
	SetPageStringE(v string) error
	SetAdapterObject(adapter Adapter)
	GetSelectFields() map[string]bool
	GetClauses() []Expr
	GetPreloads() map[string][]Expr
	GetPage() Page
	GetSort() *OrderBy
	SetSort(sort *OrderBy)
	GetAdapterObject() Adapter
	GetSqlString(ctx any, conditionType ...string) string
	GetQuery(ctx any, conditionType ...string) Query
	Build(adapter Adapter)
	BuildE(adapter Adapter) error
	Explain() string
	Clone() Figo
	Walk(visit func(Expr))
}

// Adapter renders a Figo's built state for one backend. The bool result is
// the success flag: false means the render failed (unsupported expression,
// wrong ctx type, ...) and the caller must treat the query as unrenderable —
// never as "no filters". ctx is adapter-specific (a *gorm.DB, a table name
// string, ...); conditionType optionally selects statement segments.
type Adapter interface {
	GetSqlString(f Figo, ctx any, conditionType ...string) (string, bool)
	GetQuery(f Figo, ctx any, conditionType ...string) (Query, bool)
}

type figo struct {
	clauses       []Expr
	preloads      map[string][]Expr
	page          Page
	sort          *OrderBy
	selectFields  map[string]bool
	pluginManager *PluginManager
	dsl           string
	namingFunc    NamingFunc // never nil; SnakeCaseNaming by default
	adapterObj    Adapter
	pageFromDSL   pageOrigin // WHICH page components came from a page= directive (vs SetPage); a DSL replacement resets only those
	sortFromDSL   bool       // sort came from a sort= directive (vs SetSort), same rule as pageFromDSL
	builtFromDSL  bool       // last Build materialized clause state from a DSL, so an empty-DSL rebuild must clear it

	// selectFieldsAsked is the projection the CALLER asked for, kept apart from
	// the projection actually rendered. FieldsPlugin prunes the projection from
	// a ClauseFinalizer, through the same public SetSelectFields the caller
	// uses, so the pruned set used to overwrite the request permanently: once
	// the whitelist narrowed it, widening the policy and rebuilding still
	// rendered the narrowed set. Each Build now restores the request before the
	// finalizers run, so pruning shapes one render instead of the instance.
	// clausesAsked is the caller/DSL-derived clause list, kept apart from the
	// list actually rendered — the same split selectFieldsAsked makes for the
	// projection, and for the same reason. A ClauseFinalizer may legitimately
	// replace the render with the never-true OrExpr{} (FieldsPlugin does this
	// when every projected column is forbidden), but `f.clauses = finalized`
	// made that pill the instance's authoritative list, so on a DSL-less
	// instance it became the INPUT to the next render and no public call could
	// recover it (AddFilter only appends, so the result stayed 1=0 forever).
	// Each Build now re-derives the render from this list.
	clausesAsked []Expr

	selectFieldsAsked map[string]bool
	inFinalize        bool // true while ClauseFinalizers run: SetSelectFields then writes the render only

	mu sync.RWMutex // Mutex for concurrent access protection
}

// New constructs a new instance. Supply the adapter when you build:
// Build(GormAdapter{}) (or via SetAdapterObject). New itself takes no adapter.
func New() Figo {
	return &figo{page: Page{
		Skip: 0,
		Take: 20,
	}, preloads: make(map[string][]Expr), selectFields: make(map[string]bool), selectFieldsAsked: make(map[string]bool), clauses: make([]Expr, 0), namingFunc: SnakeCaseNaming}
}

// pageOrigin records WHICH components of the Page a page= directive actually
// supplied. It used to be a single bool set on the mere PRESENCE of the token,
// before any segment was validated, so a partial or malformed directive claimed
// the caller's whole SetPage value: "page=skip:5" + SetPage(0,100) built
// {5,100} the first time and {5,20} the second, because the rebuild reset the
// Take the DSL never mentioned. Resetting per component keeps Build idempotent
// and leaves SetPage owning everything the DSL did not set. (sort= has always
// gated its ownership flag on a segment actually parsing — see :1032.)
type pageOrigin uint8

const (
	pageSkipFromDSL pageOrigin = 1 << iota
	pageTakeFromDSL
)

// resetDSLPage restores the components a page= directive owns to New()'s
// defaults, leaving caller-owned components untouched. f.mu must be held.
func (f *figo) resetDSLPage() {
	if f.pageFromDSL == 0 {
		return
	}
	def := Page{Skip: 0, Take: 20} // New()'s default
	if f.pageFromDSL&pageSkipFromDSL != 0 {
		f.page.Skip = def.Skip
	}
	if f.pageFromDSL&pageTakeFromDSL != 0 {
		f.page.Take = def.Take
	}
	f.pageFromDSL = 0
}

func (p *Page) validate() {
	if p.Skip < 0 {
		p.Skip = 0
	}
	if p.Take < 0 {
		p.Take = 0
	}
}

// Node is the parser's intermediate tree node. It is exported for historical
// reasons only — nothing outside the parser reads it, and its layout may
// change without notice. Build queries through the Figo interface and inspect
// them via GetClauses/Walk instead.
type Node struct {
	Expression []Expr
	Operator   Operation
	Value      string
	Field      string
	Children   []*Node
	Parent     *Node
}

// isDSLSpace reports whether c separates tokens in the DSL. Tabs and
// newlines count so multi-line filters parse the same as single-line ones;
// whitespace inside quoted values never reaches these checks.
func isDSLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// isSafeLookaheadValue reports whether the token after a spaced operator can
// be consumed as that operator's value. A fully quoted (or bracketed/
// parenthesized list) token is always a value — operator characters inside it
// are literal, so `name = "a=b"` keeps its value instead of silently becoming
// `name = ”`. An unquoted token containing operator characters or a logical
// keyword is not consumed (it belongs to the next expression).
func isSafeLookaheadValue(v string) bool {
	if v == "" {
		return false
	}
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') ||
			(v[0] == '[' && v[len(v)-1] == ']') ||
			(v[0] == '(' && v[len(v)-1] == ')') {
			return true
		}
	}
	if strings.ContainsAny(v, "=><!") {
		return false
	}
	return v != "and" && v != "or" && v != "not"
}

// addDiag records a parse diagnostic. diags may be nil (diagnostics are then
// discarded), which keeps every parse path identical whether the caller wants
// them (BuildE) or not (Build).
func addDiag(diags *[]error, format string, args ...any) {
	if diags != nil {
		*diags = append(*diags, fmt.Errorf(format, args...))
	}
}

// Two resource caps are checked BEFORE the scanner sees a byte, so refusing a
// hostile input never depends on surviving it. Every parser DoS to date was a
// super-linear path reached by untrusted input (nested load= re-parsing burned
// 2s CPU and 167MB heap on a 36KB filter; left-nested connector chains took
// 12s at 50k conjuncts), and each was patched pointwise after the fact — but a
// LimitsPlugin budget cannot protect the parse that MEASURES the query,
// because limits run AfterParse on the built tree. These caps sit far above
// everything the regression suite pins as supported (50k nested groups,
// 20k-conjunct chains, 40k-deep not-runs — all under 200KB): they bound the
// damage of the NEXT super-linear path, they do not police query size.
const (
	maxDSLBytes      = 1 << 20 // 1 MiB
	maxDSLGroupDepth = 100_000
)

// dslResourceGuard reports why expr must not be parsed, or "" when it is
// within the caps. The depth scan is quote-aware under the same rule as the
// tokenizer (a bare '"' toggles, no escapes), so parens inside a string
// literal do not count toward nesting.
func dslResourceGuard(expr string) string {
	if len(expr) > maxDSLBytes {
		return fmt.Sprintf("DSL is %d bytes, over the %d-byte parser cap", len(expr), maxDSLBytes)
	}
	depth, inQuote := 0, false
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '"':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				depth++
				if depth > maxDSLGroupDepth {
					return fmt.Sprintf("DSL nests groups deeper than the %d-level parser cap", maxDSLGroupDepth)
				}
			}
		case ')':
			if !inQuote && depth > 0 {
				depth--
			}
		}
	}
	return ""
}

// parseDSL scans the DSL into the Node tree. Malformed constructs are skipped
// (never fatal) exactly as they always were; each skip is additionally
// recorded into diags so BuildE can report what the built query does NOT
// include.
func (f *figo) parseDSL(expr string, diags *[]error) *Node {
	return f.parseDSLDepth(expr, diags, 0)
}

// parseDSLDepth is parseDSL with the load= nesting level. loadDepth > 0 means
// the input is the CONTENT of a load=[...] directive, where a further load= is
// not representable and must be skipped without being parsed (see the branch
// below).
func (f *figo) parseDSLDepth(expr string, diags *[]error, loadDepth int) *Node {
	root := &Node{Value: "root", Expression: make([]Expr, 0)}
	stack := []*Node{root}
	current := root
	for i := 0; i < len(expr); {
		switch expr[i] {
		case '(':
			newNode := &Node{Operator: "----", Parent: current}
			current.Children = append(current.Children, newNode)
			stack = append(stack, newNode)
			current = newNode
			i++
		case ')':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
				current = stack[len(stack)-1]
			} else {
				addDiag(diags, "unmatched ')' ignored")
			}
			i++
		case ' ', '\t', '\n', '\r':
			i++
		default:
			j := i
			ff := -1
			parenDepth := 0   // balance of '(' opened *within* this token (e.g. BETWEEN's "(10..20)")
			bracketDepth := 0 // balance of '[' for list values (<in>[...], <nin>[...])
			for j < len(expr) {

				if expr[j] == '"' && ff == -1 {
					ff = 1
					j++
					continue
				}
				if expr[j] == '"' && ff == 1 {
					ff = 0
					j++
					// A closing quote ends the token only outside a bracketed list
					// or parenthesized value. Inside either (e.g. <in>["a,b","c"],
					// <bet>("(a".."b)")) more quoted elements can follow, so keep
					// scanning and reset the quote state.
					if bracketDepth > 0 || parenDepth > 0 {
						ff = -1
						continue
					}
					break

				}

				if expr[j] != '"' && ff == 1 {
					j++
					continue
				}

				// Whitespace and the closing quote only terminate the token outside a
				// bracketed list value; inside one, "[1, 2, 3]" stays a single token.
				if isDSLSpace(expr[j]) && ff == -1 && bracketDepth == 0 {
					break
				}

				if isDSLSpace(expr[j]) && ff == 0 && bracketDepth == 0 {
					break
				}

				// Track list brackets so their commas/spaces/quoted strings don't
				// split the token prematurely.
				if expr[j] == '[' {
					bracketDepth++
					j++
					continue
				}
				if expr[j] == ']' {
					if bracketDepth > 0 {
						bracketDepth--
						j++
						// The list value is complete, so the token ends here —
						// exactly as a closing QUOTE at depth 0 ends it above.
						// Continuing instead absorbed a glued connector into the
						// value: "age<in>[1,2]and x=1" tokenized as
						// "age<in>[1,2]and", binding the garbage members "[1" and
						// "2]and" and silently downgrading the connector to AND,
						// with no diagnostic at all.
						if bracketDepth == 0 {
							break
						}
						continue
					}
					j++
					continue
				}

				// Parentheses are overloaded: they group logical expressions but
				// also delimit value syntax such as BETWEEN's "<bet>(10..20)".
				// A '(' encountered mid-token belongs to the value, so track its
				// depth. A ')' only terminates the token (leaving it for the outer
				// loop to pop the group) when it has no matching '(' in this token;
				// otherwise it closes the value's own parenthesis. Quoted parens
				// (ff == 1) are handled above and never reach here.
				if expr[j] == '(' {
					// A '(' glued to a bare logical keyword opens a group, not a
					// value: "not(a=1 or b=2)" must tokenize as "not" + group.
					// Without this break the whole run became ONE token and parsed
					// as a filter on the literal field "not(a" — silently dropping
					// the NOT (and turning "or(" into an implicit AND).
					if tok := expr[i:j]; tok == "not" || tok == "and" || tok == "or" {
						break
					}
					parenDepth++
					j++
					continue
				}
				if expr[j] == ')' {
					if parenDepth > 0 {
						parenDepth--
						j++
						// Same rule as ']' above: once the value's own
						// parenthesis is closed the token is complete.
						// "price<bet>(10..20)and b=2" used to keep scanning and
						// produce the token "price<bet>(10..20)and", whose bounds
						// typed as the strings "(10" and "20)and" while the OR/AND
						// connector was swallowed — silently, diag=nil.
						if parenDepth == 0 {
							break
						}
						continue
					}
					break
				}

				// "<null>" and "<notnull>" take no value and end in '>', so they
				// are self-delimiting: the token ends right after the operator.
				// Without this a glued keyword was absorbed into the token —
				// "deleted_at<null>or tenant_id=7" turned the OR into an implicit
				// AND, and "a<null>b=2 and c=3" dropped b=2 entirely — in both
				// cases with no diagnostic, because the operator still matched.
				if expr[j] == '>' && ff == -1 && bracketDepth == 0 && parenDepth == 0 {
					if tok := expr[i : j+1]; strings.HasSuffix(tok, string(OperationIsNull)) || strings.HasSuffix(tok, string(OperationNotNull)) {
						j++
						break
					}
				}

				j++
			}
			// A quote that never closed swallows the rest of the DSL into one
			// token, which can WIDEN the query ("not name=\"x and tenant_id=5"
			// negates one bogus predicate instead of two real ones). The value is
			// still bound (dropping the token would widen further), but BuildE
			// must say so: the spaced form was already diagnosed, the unspaced
			// form was silent.
			if ff == 1 {
				addDiag(diags, "unterminated '\"' in %q (rest of the input was absorbed into one value)", strings.TrimSpace(expr[i:j]))
			}
			token := strings.TrimSpace(expr[i:j])
			if token != "" {
				// Check if this is a logical operator (not, and, or)
				if token == "not" || token == "and" || token == "or" {
					// Handle logical operators
					var op Operation
					switch token {
					case "not":
						op = OperationNot
					case "and":
						op = OperationAnd
					case "or":
						op = OperationOr
					}

					// Create a node for the logical operator
					newNode := &Node{Operator: op, Value: token, Field: "", Parent: current, Expression: make([]Expr, 0)}
					current.Children = append(current.Children, newNode)
					i = j
					continue
				}

				// Require the '=' so ordinary field names that merely start with
				// these keywords (sortOrder, pageCount, loadedAt) are parsed as
				// filters rather than swallowed as sort/page/load directives.
				if strings.HasPrefix(token, string(OperationSort)+"=") || strings.HasPrefix(token, string(OperationPage)+"=") || strings.HasPrefix(token, string(OperationLoad)+"=") {
					// Record the directive's POSITION in the token stream (see
					// operationDirective): it is not an operand, so the precedence
					// pass has to absorb the connector written next to it and any
					// "not" written in front of it. Every branch below advances i
					// and continues, so this is the single place to mark it.
					current.Children = append(current.Children, &Node{Operator: operationDirective, Value: token, Parent: current})

					if strings.HasPrefix(token, string(OperationLoad)+"=") {
						loadLabel := fmt.Sprintf("%v=[", string(OperationLoad))
						// Without the '[' there is no bracket to balance: the scan
						// below starts at bracketCount 1 and would hunt a ']' all
						// the way to the end of the string, silently swallowing
						// every filter after the malformed directive ("load=x id=5"
						// dropped id=5 too).
						if !strings.HasPrefix(token, loadLabel) {
							addDiag(diags, "malformed load= directive %q (expected load=[Relation:filter])", token)
							i = j
							continue
						}
						// Where does the directive END? The tokenizer above already
						// answered that: it tracked bracket depth AND quote state,
						// and the ']'-at-depth-0 break means the token stops
						// exactly at the directive's own closing bracket. So take
						// its answer (j) whenever it finished balanced.
						//
						// Re-deriving the end here with a second, QUOTE-BLIND scan
						// truncated the directive at a ']' written inside a quoted
						// value: `load=[Orders:name="a]b"] and tenant_id=7` closed
						// the balance early, parsing resumed mid-value and
						// tenant_id was never parsed at all — an empty WHERE that
						// matches every row under Build(), i.e. the H6 failure mode
						// re-entered through the H6 fix itself. The rescan is kept
						// only for a token the tokenizer could NOT close (an
						// unterminated quote, or a '[' with no matching ']'), where
						// there is no trustworthy end and the balancer's verdict is
						// what produces the "unclosed load=" diagnostic. Starting
						// it at the directive's own '[' — not at j-1, the token's
						// last byte — is what H6 fixed: with a keyword glued to the
						// closing bracket ("load=[Orders:id=1]and tenant_id=7") a
						// scan from j-1 hunted for a ']' after the directive had
						// already closed and swallowed every remaining filter.
						k := j
						bracketCount := bracketDepth
						if bracketDepth != 0 || ff == 1 {
							k = i + len(loadLabel)
							bracketCount = 1
							for k < len(expr) && bracketCount > 0 {

								switch expr[k] {
								case '[':
									bracketCount++
								case ']':
									bracketCount--
								}
								k++

							}
						}

						// A load= inside a load= is not representable, and its
						// content was discarded anyway — so do not PARSE it.
						// Recursing was the entire cost: each level re-parsed the
						// whole remaining nesting on a scratch instance, so a
						// ~36 KB filter of nested load= burned 2 s of CPU and
						// ~167 MB of LIVE heap to produce one clause and one empty
						// preload. No QueryLimits setting could reject it either,
						// because limits measure the built tree AfterParse and that
						// tree is trivial.
						if loadDepth > 0 {
							addDiag(diags, "nested load= inside load=[...] is not supported and was ignored (%q)", token)
							i = k
							continue
						}

						v := strings.TrimSpace(expr[i:k])
						labelIndex := strings.Index(v, loadLabel)
						if labelIndex == -1 {
							addDiag(diags, "malformed load= directive %q (expected load=[Relation:filter])", v)
							i = k
							continue
						}
						// The content slice below assumes v ends with ']'. Guard the
						// bounds so a malformed/unclosed bracket (e.g. "load=[") is
						// skipped rather than panicking with slice-out-of-range.
						start := labelIndex + len(loadLabel)
						end := len(v) - 1
						if bracketCount != 0 || !strings.HasSuffix(v, "]") || start > end {
							addDiag(diags, "unclosed load= directive %q", v)
							i = k
							continue
						}
						content := v[start:end]
						if content == "" {
							addDiag(diags, "empty load= directive")
							i = k
							continue
						}

						// Segments split on '|' OUTSIDE quotes: a plain split cut
						// load=[Orders:name="x|y"] inside the quoted value.
						loadSplit := splitOutsideQuotes(content, '|')
						for _, l := range loadSplit {
							colonIndex := strings.Index(l, ":")
							if colonIndex == -1 {
								if strings.TrimSpace(l) != "" {
									addDiag(diags, "malformed load= segment %q (missing ':' between relation and filter)", l)
								}
								continue
							}
							rawTable := l[:colonIndex]
							table := strings.TrimSpace(rawTable)
							if table == "" {
								// An empty relation name rendered JOIN ""/Preload("").
								addDiag(diags, "load= segment %q has an empty relation name", l)
								continue
							}
							loadContent := strings.TrimSpace(l[colonIndex+1:])

							// Parse the relation filter on a scratch instance: the
							// content is a full DSL expression, so without isolation
							// a sort=/page=/load= directive written inside it would
							// mutate the OUTER query (a preload's sort= used to
							// become the main query's ORDER BY, its page= the main
							// LIMIT/OFFSET, and a nested load= a top-level preload).
							// Preloads have no sort/page/nested-load representation,
							// so those directives are dropped with a diagnostic.
							scratch := &figo{preloads: make(map[string][]Expr), selectFields: make(map[string]bool), namingFunc: f.namingFunc}
							loadRootNode := scratch.parseDSLDepth(loadContent, diags, loadDepth+1)
							if scratch.sort != nil {
								addDiag(diags, "sort= inside load=[%s:...] is not supported and was ignored", table)
							}
							if scratch.pageFromDSL != 0 {
								// Presence is tracked by flag: comparing the page
								// against its zero value missed page=skip:0,take:0.
								addDiag(diags, "page= inside load=[%s:...] is not supported and was ignored", table)
							}
							// (A nested load= is reported by the depth guard above,
							// which skips it without parsing, so scratch.preloads
							// can no longer become non-empty.)
							expressionParser(loadRootNode, diags)
							loadExpr := getFinalExpr(*loadRootNode)
							if loadExpr != nil {
								f.preloads[table] = append(f.preloads[table], loadExpr)
							} else {
								// A segment whose filter yields no conditions still
								// preloads the relation: dropping it entirely turned
								// "load=[T:sort=id:desc]" into no preload at all,
								// while the diag above claimed only the sort had
								// been ignored.
								if _, ok := f.preloads[table]; !ok {
									f.preloads[table] = []Expr{}
								}
								addDiag(diags, "load=[%s:...] filter %q produced no conditions; relation preloaded unfiltered", table, loadContent)
							}

						}
						i = k
						continue

					} else if strings.HasPrefix(token, string(OperationPage)+"=") {

						pageLabel := fmt.Sprintf("%v=", string(OperationPage))
						content := token[strings.Index(token, pageLabel)+len(pageLabel):]

						// Ownership is claimed per COMPONENT, and only once a
						// segment actually parses (see pageOrigin): flagging the
						// whole Page on the mere presence of the token handed a
						// malformed "page=garbage" — and a partial "page=skip:5" —
						// control of a caller's SetPage Take, which the next
						// rebuild then reset. sort= has always gated its flag the
						// same way (:1032).
						pageContent := strings.Split(content, ",")

						for _, s := range pageContent {
							pageSplit := strings.Split(s, ":")
							if len(pageSplit) != 2 {
								if strings.TrimSpace(s) != "" {
									addDiag(diags, "malformed page= segment %q (expected skip:N or take:N)", s)
								}
								continue
							}

							field := pageSplit[0]
							value := pageSplit[1]

							parseInt, parsErr := strconv.ParseInt(value, 10, 64)
							if parsErr == nil {

								switch field {
								case "skip":
									f.page.Skip = int(parseInt)
									f.pageFromDSL |= pageSkipFromDSL
								case "take":
									f.page.Take = int(parseInt)
									f.pageFromDSL |= pageTakeFromDSL
								default:
									addDiag(diags, "unknown page= key %q (expected skip or take)", field)
								}

								f.page.validate()
							} else {
								addDiag(diags, "invalid page= value %q for %q (expected an integer)", value, field)
							}

						}
						// Advance past the whole page token. (k = j-1 would re-scan
						// the token's last char; when it is '=' that produced a bogus
						// empty-field EqExpr.)
						i = j
						continue

					} else if strings.HasPrefix(token, string(OperationSort)+"=") {

						sortLabel := fmt.Sprintf("%v=", string(OperationSort))
						content := token[strings.Index(token, sortLabel)+len(sortLabel):]

						if strings.TrimSpace(content) == "" {
							addDiag(diags, "empty sort= directive")
							i = j
							continue
						}

						sortContent := strings.Split(content, ",")

						var c []OrderByColumn

						for _, s := range sortContent {
							sortSplit := strings.Split(s, ":")
							if len(sortSplit) != 2 {
								if strings.TrimSpace(s) != "" {
									addDiag(diags, "malformed sort= segment %q (expected field:asc or field:desc)", s)
								}
								continue
							}

							field := sortSplit[0]
							value := sortSplit[1]

							if strings.TrimSpace(field) == "" {
								// An empty field rendered ORDER BY "" — invalid
								// SQL on every adapter.
								addDiag(diags, "sort= segment %q has an empty field name", s)
								continue
							}
							if dir := strings.ToLower(value); dir != "asc" && dir != "desc" {
								addDiag(diags, "invalid sort direction %q for field %q (expected asc or desc)", value, field)
							}
							c = append(c, OrderByColumn{
								Name: f.parsFieldsName(field),
								Desc: strings.ToLower(value) == "desc",
							})

						}

						// Only adopt a sort that produced at least one column: a
						// directive whose every segment was malformed used to
						// replace an earlier VALID sort with an empty one.
						if len(c) > 0 {
							// Last-wins is a defensible policy; discarding a
							// fully VALID earlier directive without saying so is
							// not — BuildE promises a nil error means the built
							// query expresses all of the input, and the columns
							// are expressible (sort=a:asc,b:desc keeps both).
							if f.sortFromDSL {
								addDiag(diags, "sort= directive %q overrides an earlier sort= in the same DSL", token)
							}
							sortExpr := OrderBy{
								Columns: c,
							}
							f.sort = &sortExpr
							// Flag the sort as DSL-derived only once it is
							// actually adopted, so a directive whose every
							// segment was malformed leaves a SetSort value
							// (and its caller-owned flag) alone.
							f.sortFromDSL = true
						}
						// Advance past the whole sort token (see the page branch).
						i = j
						continue

					}

					// Unreachable: the enclosing condition guarantees the token
					// starts with load=, page= or sort=, and every one of those
					// branches advances i and continues.
				} else {
					// Try to combine tokens for expressions like "field > value" or "field =^ value"
					// Only do this for very specific cases to avoid interfering with complex operators
					combinedToken := token
					// Only combine if the token looks like a simple field name (alphanumeric + underscores)
					// and doesn't contain any operators or special characters
					if isSimpleFieldName(token) {
						// This looks like a field name with underscores, try to combine with next tokens
						nextStart := j
						for nextStart < len(expr) && isDSLSpace(expr[nextStart]) {
							nextStart++
						}
						if nextStart < len(expr) {
							nextEnd := nextStart
							var nextToken string
							// Take the operator by LONGEST MATCH rather than by
							// scanning to the next space. The scan below stops only
							// at whitespace and quotes, so a half-spaced predicate
							// ("price =10", "price <in>[10,15]", or a spaced
							// "tag <null>" that merely happens to sit before ')')
							// produced a token that failed the exact-match list and
							// was dropped WHOLE — widening a conjunct. The operator
							// spellings are self-delimiting, so matching them
							// directly ends the token in exactly the right place.
							if op := matchOperatorPrefix(expr[nextStart:]); op != "" {
								nextToken = op
								nextEnd = nextStart + len(op)
							} else {
								nextFF := -1
								for nextEnd < len(expr) {
									if expr[nextEnd] == '"' && nextFF == -1 {
										nextFF = 1
										nextEnd++
										continue
									}
									if expr[nextEnd] == '"' && nextFF == 1 {
										nextFF = 0
										nextEnd++
										break
									}
									if expr[nextEnd] != '"' && nextFF == 1 {
										nextEnd++
										continue
									}
									if isDSLSpace(expr[nextEnd]) && nextFF == -1 {
										break
									}
									if isDSLSpace(expr[nextEnd]) && nextFF == 0 {
										break
									}
									nextEnd++
								}
								nextToken = strings.TrimSpace(expr[nextStart:nextEnd])
							}
							// Combine with both simple and complex operators
							if nextToken == ">" || nextToken == "<" || nextToken == "=" || nextToken == "!=" || nextToken == ">=" || nextToken == "<=" || nextToken == "=^" || nextToken == "!=^" || nextToken == ".=^" || nextToken == "=~" || nextToken == "!=~" || nextToken == "<in>" || nextToken == "<nin>" || nextToken == "<bet>" || nextToken == "<null>" || nextToken == "<notnull>" {
								combinedToken = token + " " + nextToken
								j = nextEnd

								// Try to get the value token as well
								if nextToken == "<bet>" || nextToken == "<in>" || nextToken == "<nin>" {
									valueStart := j
									for valueStart < len(expr) && isDSLSpace(expr[valueStart]) {
										valueStart++
									}
									if valueStart < len(expr) {
										valueEnd := valueStart
										valueFF := -1
										parenCount := 0
										bracketCount := 0
										for valueEnd < len(expr) {
											if expr[valueEnd] == '"' && valueFF == -1 {
												valueFF = 1
												valueEnd++
												continue
											}
											if expr[valueEnd] == '"' && valueFF == 1 {
												valueFF = 0
												valueEnd++
												// Inside a bracketed list or parenthesized
												// range more quoted elements may follow
												// (<in>["a b","c"], <bet>("a..b".."z")); a
												// bare quoted BETWEEN bound may continue
												// with the ".." range separator.
												if bracketCount > 0 || parenCount > 0 || strings.HasPrefix(expr[valueEnd:], "..") {
													valueFF = -1
													continue
												}
												break
											}
											if expr[valueEnd] != '"' && valueFF == 1 {
												valueEnd++
												continue
											}
											// Track list brackets so "[1, 2]" (spaces
											// inside) stays one value token.
											if expr[valueEnd] == '[' && valueFF == -1 {
												bracketCount++
											}
											if expr[valueEnd] == ']' && valueFF == -1 {
												bracketCount--
												if bracketCount == 0 {
													valueEnd++
													break
												}
											}
											// Handle parentheses for BETWEEN operations
											if expr[valueEnd] == '(' && valueFF == -1 {
												parenCount++
											}
											if expr[valueEnd] == ')' && valueFF == -1 && bracketCount == 0 {
												parenCount--
												if parenCount == 0 {
													valueEnd++
													break
												}
											}
											// Stop at whitespace, parentheses, or logical operators (but not inside parentheses/brackets)
											if (isDSLSpace(expr[valueEnd]) || expr[valueEnd] == ')' || expr[valueEnd] == '(') && valueFF == -1 && parenCount == 0 && bracketCount == 0 {
												break
											}
											if isDSLSpace(expr[valueEnd]) && valueFF == 0 && parenCount == 0 && bracketCount == 0 {
												break
											}
											valueEnd++
										}
										valueToken := strings.TrimSpace(expr[valueStart:valueEnd])
										if isSafeLookaheadValue(valueToken) {
											combinedToken = combinedToken + " " + valueToken
											j = valueEnd
										}
									}
								} else {
									// For simple operators, use simpler value extraction
									valueStart := j
									for valueStart < len(expr) && isDSLSpace(expr[valueStart]) {
										valueStart++
									}
									if valueStart < len(expr) {
										valueEnd := valueStart
										valueFF := -1
										for valueEnd < len(expr) {
											if expr[valueEnd] == '"' && valueFF == -1 {
												valueFF = 1
												valueEnd++
												continue
											}
											if expr[valueEnd] == '"' && valueFF == 1 {
												valueFF = 0
												valueEnd++
												break
											}
											if expr[valueEnd] != '"' && valueFF == 1 {
												valueEnd++
												continue
											}
											// Stop at whitespace, parentheses, or logical operators
											if (isDSLSpace(expr[valueEnd]) || expr[valueEnd] == ')' || expr[valueEnd] == '(') && valueFF == -1 {
												break
											}
											if isDSLSpace(expr[valueEnd]) && valueFF == 0 {
												break
											}
											valueEnd++
										}
										valueToken := strings.TrimSpace(expr[valueStart:valueEnd])
										if isSafeLookaheadValue(valueToken) {
											combinedToken = combinedToken + " " + valueToken
											j = valueEnd
										}
									}
								}
							}
						}
					}

					operator, valueStr, field := parseToken(combinedToken)

					// Bare and/or/not tokens were consumed above, so a token
					// without a recognizable operator can never be a logical
					// node here. In particular a *value* equal to "and"/"or"/
					// "not" (e.g. name="and") must stay an ordinary value and
					// must never overwrite the node's operator.
					if operator == "" {
						addDiag(diags, "unrecognized token %q (no operator found)", combinedToken)
						dropDanglingNot(current, diags)
						i = j
						continue
					}
					if field == "" {
						addDiag(diags, "operator %q with no field name (token %q)", operator, combinedToken)
						dropDanglingNot(current, diags)
						i = j
						continue
					}
					if strings.TrimSpace(valueStr) == "" && operator != OperationIsNull && operator != OperationNotNull {
						// A bare "name =" built the almost-certainly unintended
						// comparison `name` = '' with no diagnostic; an explicit
						// empty string is spelled name="".
						addDiag(diags, "operator %q on field %q has no value", operator, field)
						dropDanglingNot(current, diags)
						i = j
						continue
					}

					// Ignore-field filtering happens on the built expression
					// tree in Build (pruning a condition there cannot leave an
					// orphaned and/or/not behind, unlike skipping the node here).

					// Pass the raw literal (quotes intact): getClausesFromOperation
					// types each literal exactly once. Parsing here and re-parsing
					// there through a %v round-trip destroyed quoted-string typing
					// ("0123" became int64 123), nulls and dates.
					convertedField := f.parsFieldsName(field)
					clauseExpr := getClausesFromOperation(operator, convertedField, valueStr, diags)
					if clauseExpr == nil {
						addDiag(diags, "invalid value %q for operator %q on field %q", valueStr, operator, field)
						// The node used to be appended anyway, carrying a nil
						// expression the precedence pass skips WITHOUT consuming a
						// pending "not" — which then negated the NEXT (valid)
						// predicate: "not price<bet>(broken) and y=1" rendered
						// NOT(y=1).
						dropDanglingNot(current, diags)
						i = j
						continue
					}
					newNode := &Node{Operator: operator, Value: valueStr, Field: convertedField, Parent: current, Expression: make([]Expr, 0)}
					newNode.Expression = append(newNode.Expression, clauseExpr)
					current.Children = append(current.Children, newNode)
					i = j
				}
			} else {
				i = j
			}
		}
	}

	if len(stack) > 1 {
		addDiag(diags, "%d unclosed '(' group(s) auto-closed at end of input", len(stack)-1)
	}

	return root
}

// dropDanglingNot removes trailing "not" operator nodes left behind when the
// condition they were about to negate is dropped as invalid. Left in place,
// the not would attach to the NEXT predicate and invert its meaning.
func dropDanglingNot(current *Node, diags *[]error) {
	for len(current.Children) > 0 {
		last := current.Children[len(current.Children)-1]
		if last.Operator != OperationNot {
			return
		}
		current.Children = current.Children[:len(current.Children)-1]
		addDiag(diags, "dropped 'not' preceding an invalid condition")
	}
}

func (f *figo) parsFieldsName(str string) string {
	return convertFieldName(f.namingFunc, str)
}

// convertFieldName applies a naming func to a field name (nil-safe: a nil
// func leaves the name unchanged). Shared by the parser (via parsFieldsName)
// and plugins that need to match field names in their converted form
// (e.g. FieldsPlugin's ignore matching).
func convertFieldName(fn NamingFunc, str string) string {
	if fn != nil {
		return fn(str)
	}
	return str
}

// ParseValue types a single DSL literal exactly the way the parser types
// filter values: quoted literals stay strings verbatim; unquoted literals get
// bool/null/int64/float64/date detection. Use it to coerce a value outside
// the DSL (e.g. one incoming parameter) the way figo would — a=1 and a="1"
// render different SQL, so matching the DSL's typing matters.
func ParseValue(str string) any {
	return parseScalarLiteral(str)
}

// mayBeDate is a cheap shape gate in front of parseDate's layout probe. The
// probe runs on EVERY unquoted literal that survives numeric typing —
// including each element of an IN list — and trying 11 layouts against input
// that cannot possibly be a date is pure waste. It is strictly conservative:
// every layout below either starts with a digit and carries a '-' or '/'
// separator, or starts with a month name (matched case-insensitively by
// time.Parse) and carries a comma, and nothing under 10 or over 64 bytes
// parses under any of them (the shortest is "2006-01-02", the longest a
// nanosecond RFC3339 timestamp — fractions past 9 digits are a parse error).
func mayBeDate(s string) bool {
	if len(s) < 10 || len(s) > 64 {
		return false
	}
	switch c := s[0]; {
	case c >= '0' && c <= '9':
		return strings.ContainsAny(s, "-/")
	case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
		return strings.IndexByte(s, ',') >= 0
	}
	return false
}

// parseDate attempts to parse a string as a date using common formats
func parseDate(s string) (time.Time, error) {
	if !mayBeDate(s) {
		return time.Time{}, fmt.Errorf("unable to parse date: %s", s)
	}
	// Common date formats to try
	formats := []string{
		time.RFC3339,           // 2006-01-02T15:04:05Z07:00
		time.RFC3339Nano,       // 2006-01-02T15:04:05.999999999Z07:00
		"2006-01-02T15:04:05Z", // 2006-01-02T15:04:05Z
		"2006-01-02T15:04:05",  // 2006-01-02T15:04:05
		"2006-01-02 15:04:05",  // 2006-01-02 15:04:05
		"2006-01-02",           // 2006-01-02
		"2006/01/02",           // 2006/01/02
		"01/02/2006",           // 01/02/2006 (US format)
		"02/01/2006",           // 02/01/2006 (EU format)
		"Jan 2, 2006",          // Jan 2, 2006
		"January 2, 2006",      // January 2, 2006
	}

	for _, format := range formats {
		if t, err := time.Parse(format, s); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse date: %s", s)
}

// isSimpleFieldName checks if a token looks like a simple field name
func isSimpleFieldName(token string) bool {
	// Must not be empty
	if token == "" {
		return false
	}

	// Must not contain operators or special characters
	if strings.Contains(token, "=") || strings.Contains(token, ">") || strings.Contains(token, "<") ||
		strings.Contains(token, "!") || strings.Contains(token, "^") || strings.Contains(token, "~") ||
		strings.Contains(token, "page=") || strings.Contains(token, "sort=") || strings.Contains(token, "load=") {
		return false
	}

	// Must not be logical operators
	if token == "and" || token == "or" || token == "not" {
		return false
	}

	// Must not be complex operators
	if token == "like" || token == "in" || token == "between" || token == "null" || token == "notnull" {
		return false
	}

	// Must contain only field-name characters: any Unicode letter or digit
	// (not just ASCII — an ASCII-only check made a non-Latin name followed by
	// a *spaced* operator, e.g. `سن > 5`, fail to combine and emit a predicate
	// on an empty column), plus '_', '.', '-' and '$' so qualified/kebab/
	// Mongo-style names (user.age, user-name, price$) are recognized as fields.
	// Operator characters (= > < ! ^ ~) are already rejected by the guard
	// above, so widening here can't swallow an operator token.
	for _, char := range token {
		if !(unicode.IsLetter(char) || unicode.IsDigit(char) ||
			char == '_' || char == '.' || char == '-' || char == '$') {
			return false
		}
	}

	return true
}

// spacedOperators lists the operator spellings the "field <op> value"
// leniency recognizes, LONGEST FIRST so matchOperatorPrefix takes the longest
// match ("<notnull>" before "<null>" before "<", "!=~" before "!=").
var spacedOperators = []string{
	"<notnull>", "<null>", "<bet>", "<nin>", "<in>",
	"!=~", "!=^", ".=^",
	">=", "<=", "!=", "=^", "=~",
	">", "<", "=",
}

// matchOperatorPrefix returns the longest operator spelling s starts with, or
// "" if it starts with none.
func matchOperatorPrefix(s string) string {
	for _, op := range spacedOperators {
		if strings.HasPrefix(s, op) {
			return op
		}
	}
	return ""
}

func parseToken(token string) (Operation, string, string) {
	// Order matters: place custom multi-char markers first
	operators := []Operation{
		OperationNotRegex,
		OperationRegex,
		OperationNotLike,
		OperationILike,
		OperationNotIn,
		OperationIn,
		OperationBetween,
		OperationNotNull,
		OperationIsNull,
		OperationLike,
		OperationGte, OperationLte,
		OperationNeq, OperationGt, OperationLt, OperationEq,
	}
	for _, op := range operators {
		// Match the operator only OUTSIDE quotes, so operator characters inside a
		// quoted value (name="x=y", url="http://h?a=1") don't split the token.
		if idx := indexOutsideQuotes(token, string(op)); idx >= 0 {
			field := strings.TrimSpace(token[:idx])
			right := token[idx+len(string(op)):]
			return op, right, field
		}
	}
	return "", token, ""
}

// indexOutsideQuotes returns the first index of substr in s that lies outside a
// double-quoted region, or -1 if there is none.
func indexOutsideQuotes(s, substr string) int {
	if substr == "" {
		return -1
	}
	inQuote := false
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i] == '"' {
			inQuote = !inQuote
			continue
		}
		if !inQuote && strings.HasPrefix(s[i:], substr) {
			return i
		}
	}
	return -1
}

// splitOutsideQuotes splits s on sep, ignoring separators inside double quotes.
func splitOutsideQuotes(s string, sep byte) []string {
	var parts []string
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			inQuote = !inQuote
			b.WriteByte(c)
			continue
		}
		if c == sep && !inQuote {
			parts = append(parts, b.String())
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	parts = append(parts, b.String())
	return parts
}

// unquoteLiteral strips one balanced pair of surrounding double quotes.
func unquoteLiteral(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1], true
	}
	return s, false
}

// parseScalarLiteral types a single DSL literal. A quoted literal is always
// the enclosed string, verbatim — quoting is the caller's way of saying
// "do not re-type this" ("0123" stays "0123", "true" stays a string).
// Unquoted literals get bool/null/int64/float64/date detection.
func parseScalarLiteral(raw string) any {
	s, quoted := unquoteLiteral(raw)
	if quoted {
		return s
	}
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	if s == "null" || s == "NULL" {
		return nil
	}
	if s != "" {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
		// An integer too large for int64 must not silently degrade to a
		// lossy float64; leave it as a string for the database to coerce.
		if isAllDigits(s) {
			return s
		}
		// strconv.ParseFloat accepts GO's literal grammar, not SQL/JSON's:
		// digit separators ("1_000") and hex floats ("0x1p4") sailed past the
		// isAllDigits guard above and degraded to a lossy float64, so
		// "id=9_007_199_254_740_993" bound 9007199254740992 and returned a
		// DIFFERENT record with BuildE nil. ParseInt above uses an explicit
		// base 10 and already rejects them, which is why this was the only
		// leak. Such literals stay strings, like oversized integers.
		// (The check gates only the FLOAT path — month names such as
		// "September 5, 2024" contain a 'p' and must still reach parseDate.)
		if !strings.ContainsAny(s, "_xXpP") {
			if f64, err := strconv.ParseFloat(s, 64); err == nil {
				// NaN/Inf have no SQL/BSON/JSON literal — keep the raw text as a
				// string (the same policy as oversized integers) rather than emit
				// a value no backend can render.
				if !math.IsNaN(f64) && !math.IsInf(f64, 0) {
					return f64
				}
			}
		}
		if dateVal, err := parseDate(s); err == nil {
			return dateVal
		}
	}
	return s
}

// isNumericLiteral / isStringLiteral classify a typed DSL literal for the
// <bet> bound-compatibility check.
func isNumericLiteral(v any) bool {
	switch v.(type) {
	case int64, float64:
		return true
	}
	return false
}

func isStringLiteral(v any) bool {
	_, ok := v.(string)
	return ok
}

func isAllDigits(s string) bool {
	t := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	if t == "" {
		return false
	}
	for _, c := range t {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseListLiteral parses a list literal like [1,2,"x"] or ["a,b","c"].
// Parenthesized lists (<in>(1,2)) are accepted too — leaving the parens in
// place corrupted the first and last element into "(1" and "2)".
func parseListLiteral(raw string, diags *[]error) []any {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
	} else if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		s = strings.TrimPrefix(s, "(")
		s = strings.TrimSuffix(s, ")")
	}
	if s == "" {
		return nil
	}
	// Split on commas OUTSIDE quotes so quoted elements can contain commas.
	parts := splitOutsideQuotes(s, ',')
	vals := make([]any, 0, len(parts))
	for _, p := range parts {
		// A blank part (leading/trailing/doubled comma) is a separator
		// artifact, not a value: it used to be typed as "" and injected into
		// the predicate, so `name<in>["alice",]` matched every empty-string row
		// as well and the mirrored `<nin>` silently HID them — both with a nil
		// diagnostic. An explicit empty-string member is still spellable as "",
		// so skipping blanks loses no expressiveness. (The empty LIST is
		// already special-cased above, yielding the fail-closed 1=0 sentinel.)
		if strings.TrimSpace(p) == "" {
			addDiag(diags, "empty element in list %q ignored (write \"\" for an explicit empty string)", raw)
			continue
		}
		vals = append(vals, parseScalarLiteral(p))
	}
	if len(vals) == 0 {
		return nil
	}
	return vals
}

func getClausesFromOperation(o Operation, field string, value any, diags *[]error) Expr {
	// The DSL parser passes the raw literal (quotes intact) so each literal
	// is typed exactly once, here. Programmatic callers may pass an already
	// typed value, which is used as-is.
	rawStr, isRaw := value.(string)

	scalar := func() any {
		if isRaw {
			return parseScalarLiteral(rawStr)
		}
		return value
	}
	str := func() string {
		if isRaw {
			s, _ := unquoteLiteral(rawStr)
			return s
		}
		return fmt.Sprintf("%v", value)
	}
	list := func() []any {
		if isRaw {
			return parseListLiteral(rawStr, diags)
		}
		if vals, ok := value.([]any); ok {
			return vals
		}
		return parseListLiteral(fmt.Sprintf("%v", value), diags)
	}

	switch o {
	case OperationEq:
		v := scalar()
		if v == nil {
			// x=null means "x IS NULL", not a comparison against the
			// literal string "<nil>".
			return IsNullExpr{Field: field}
		}
		return EqExpr{Field: field, Value: v}
	case OperationGte:
		return GteExpr{Field: field, Value: scalar()}
	case OperationGt:
		return GtExpr{Field: field, Value: scalar()}
	case OperationLt:
		return LtExpr{Field: field, Value: scalar()}
	case OperationLte:
		return LteExpr{Field: field, Value: scalar()}
	case OperationNeq:
		v := scalar()
		if v == nil {
			// x!=null means "x IS NOT NULL".
			return NotNullExpr{Field: field}
		}
		return NeqExpr{Field: field, Value: v}
	case OperationLike:
		return LikeExpr{Field: field, Value: str()}
	case OperationNotLike:
		return NotExpr{Operands: []Expr{LikeExpr{Field: field, Value: str()}}}
	case OperationRegex:
		return RegexExpr{Field: field, Value: str()}
	case OperationNotRegex:
		return NotExpr{Operands: []Expr{RegexExpr{Field: field, Value: str()}}}
	case OperationIn:
		return InExpr{Field: field, Values: list()}
	case OperationNotIn:
		return NotInExpr{Field: field, Values: list()}
	case OperationBetween:
		s := strings.TrimSpace(fmt.Sprintf("%v", value))
		// strip optional parentheses
		if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
			s = strings.TrimPrefix(s, "(")
			s = strings.TrimSuffix(s, ")")
		}
		// The bound separator must be found OUTSIDE quotes: a quoted low bound
		// containing ".." (code <bet> "a..b".."c") split inside the value.
		if idx := indexOutsideQuotes(s, ".."); idx > 0 {
			low := strings.TrimSpace(s[:idx])
			high := strings.TrimSpace(s[idx+2:])
			// Only the LOW bound was ever checked (that is all `idx > 0` says).
			// An absent or malformed HIGH bound was handed to parseScalarLiteral
			// and became a string: `<bet>(5..)` bound "" and `<bet>(5....100)`
			// bound "..100". SQLite, MySQL and BSON all order every number below
			// every string, so the ceiling evaporated and the range silently
			// became `>= low` — with BuildE returning nil. Reject both, exactly
			// as an empty LOW bound already is; a leading '.' also catches
			// `(1...5)`, whose "high" is the unintended 0.5.
			if low == "" || high == "" || strings.HasPrefix(high, ".") {
				return nil
			}
			lowVal, highVal := parseScalarLiteral(low), parseScalarLiteral(high)
			// A number paired with a string is the same silent widening one step
			// later (`<bet>(10..abc)` renders BETWEEN 10 AND 'abc'), so fail
			// closed on mixed kinds rather than emit a comparison whose meaning
			// depends on the engine's type ordering.
			if isNumericLiteral(lowVal) != isNumericLiteral(highVal) &&
				(isStringLiteral(lowVal) || isStringLiteral(highVal)) {
				return nil
			}
			return BetweenExpr{Field: field, Low: lowVal, High: highVal}
		}
		return nil
	case OperationILike:
		return ILikeExpr{Field: field, Value: str()}
	case OperationIsNull:
		return IsNullExpr{Field: field}
	case OperationNotNull:
		return NotNullExpr{Field: field}
	default:
		return nil
	}
}

// expressionParser walks the node tree bottom-up, turning each node's children
// into that node's Expression. Every node is visited EXACTLY once: a second
// recursion pass (previously run for non-group nodes, duplicating the loop
// below) re-entered the whole subtree, so each level appended its child's
// expression list to its own a second time. The lists grew one entry per level
// and every append copied them, making a parse quadratic in group-nesting
// depth — "((((...a=1...))))" 20 000 deep took a full second, 100 000 deep did
// not finish. LimitsPlugin cannot bound this: its AfterParse hook has to Build
// (i.e. parse) before it can measure anything.
func expressionParser(node *Node, diags *[]error) {
	// Build every group child's expression before combining them here.
	for _, child := range node.Children {
		if child.Operator == OperationChild {
			expressionParser(child, diags)
		}
	}

	// A group wrapping a single child is that child: adopt its expression as-is
	// rather than re-running the precedence pass over one item.
	if node.Operator == OperationChild && len(node.Children) == 1 {
		node.Expression = append(node.Expression, node.Children[0].Expression...)
		return
	}

	if expr := buildExpressionTreeWithPrecedence(node.Children, diags); expr != nil {
		node.Expression = append(node.Expression, expr)
	}
}

// nextSlotKind classifies what a non-operand slot's right-hand neighbour
// contributes: see nextItemKind.
type nextSlotKind int

const (
	nextIsNothing nextSlotKind = iota
	nextIsOperand
	nextIsConnector
)

// nextItemKind reports what the next child that CONTRIBUTES to the item list is
// — an operand, an and/or connector, or nothing at all — plus that connector's
// operation. Slots contributing neither (another directive, a group that
// produced no expression) are skipped: each reconciles its own neighbours. A
// "not" counts as an operand, because it binds the expression that follows it,
// so a connector written before it is not dangling.
func nextItemKind(children []*Node, from int) (nextSlotKind, Operation) {
	for _, child := range children[from:] {
		switch {
		case child.Operator == operationDirective:
			continue
		case child.Operator == OperationAnd || child.Operator == OperationOr:
			return nextIsConnector, child.Operator
		case child.Operator == OperationNot:
			return nextIsOperand, OperationNot
		case len(child.Expression) > 0:
			return nextIsOperand, ""
		}
	}
	return nextIsNothing, ""
}

// buildExpressionTreeWithPrecedence builds a proper expression tree respecting operator precedence
func buildExpressionTreeWithPrecedence(children []*Node, diags *[]error) Expr {
	if len(children) == 0 {
		return nil
	}

	// Build a list of expressions and operators in order
	var items []any // Can be Expr or Operation

	// Set when a non-operand slot left the connector written AFTER it
	// ("sort=id:asc and a=1") as the one to absorb.
	absorbNextConnector := false

	// absorbAroundNonOperand is shared by the two kinds of slot that occupy a
	// token position without contributing an operand: a sort=/page=/load=
	// directive, and a parenthesized group that produced no expression. Both
	// have to reconcile the connectors written around them, and they used to do
	// it differently — the directive branch dropped the preceding connector
	// UNCONDITIONALLY (so "a=1 or sort=id:asc b=2" lost the user's only
	// connector and rendered a AND b, silently narrowing a disjunction with no
	// diagnostic on any channel) while the group branch absorbed nothing (so
	// "a=1 or (sort=id:asc) or b=2" left a connector unpaired and BuildE
	// reported a "dangling connector" error for valid DSL — the very symptom
	// M12 was filed for, surviving in the parenthesized spelling).
	//
	// The rule: absorb a connector only when it would otherwise DANGLE.
	//   * one operand on each side  -> the connector is the user's operator; keep it.
	//   * connectors on both sides  -> exactly one must go. Drop the AND and keep
	//     the OR, which is the pairing the precedence reduction below would have
	//     produced anyway (its AND pass finds no operand to pair with the second
	//     connector and drops it), so the parse is unchanged and only the
	//     spurious diagnostic is gone.
	//   * nothing left to pair with -> drop it (leading "sort=x and a=1",
	//     trailing "a=1 or sort=x").
	absorbAroundNonOperand := func(idx int) {
		prevOp, hasPrevOp := OperationAnd, false
		if n := len(items); n > 0 {
			if op, isOp := items[n-1].(Operation); isOp && (op == OperationAnd || op == OperationOr) {
				prevOp, hasPrevOp = op, true
			}
		}
		kind, nextOp := nextItemKind(children, idx+1)
		switch {
		case hasPrevOp && kind == nextIsConnector:
			if prevOp == OperationOr && nextOp == OperationAnd {
				absorbNextConnector = true
				return
			}
			items = items[:len(items)-1]
		case hasPrevOp && kind == nextIsNothing:
			items = items[:len(items)-1]
		case !hasPrevOp && len(items) == 0 && kind == nextIsConnector:
			absorbNextConnector = true
		}
	}

	for idx, child := range children {
		// A sort=/page=/load= directive occupies a token slot but is not an
		// operand (see operationDirective). It has to reconcile the connectors
		// written around it (absorbAroundNonOperand) and swallow any "not"
		// written in front of it, which otherwise jumped onto the NEXT predicate
		// and inverted a filter the user never negated ("not sort=id:asc and
		// a=1" rendered NOT(a=1); round 4 fixed only the parenthesized form).
		if child.Operator == operationDirective {
			for len(items) > 0 {
				op, isOp := items[len(items)-1].(Operation)
				if !isOp || op != OperationNot {
					break
				}
				items = items[:len(items)-1]
				addDiag(diags, "dropped 'not' preceding a %q directive", child.Value)
			}
			absorbAroundNonOperand(idx)
			continue
		}
		if absorbNextConnector {
			absorbNextConnector = false
			if child.Operator == OperationAnd || child.Operator == OperationOr {
				continue
			}
		}

		// Add expressions from this child
		if len(child.Expression) > 0 {
			items = append(items, child.Expression[len(child.Expression)-1])
		} else if child.Operator == OperationChild {
			// A parenthesized group that produced NO expression — "()", a group
			// holding only sort=/page=/load= directives, or one whose every
			// condition was invalid — contributes nothing here. A "not" written
			// in front of it would then land next to the FOLLOWING expression
			// and negate that instead: "not (sort=id:desc) and a=1" rendered
			// NOT(a=1), inverting a filter the user never negated (and, for
			// "not () a=1", with no diagnostic at all). The group consumes its
			// pending nots instead. dropDanglingNot handles the analogous case
			// for a dropped TOKEN; this is the group form.
			for len(items) > 0 {
				op, isOp := items[len(items)-1].(Operation)
				if !isOp || op != OperationNot {
					break
				}
				items = items[:len(items)-1]
				addDiag(diags, "dropped 'not' preceding a group with no conditions")
			}
			// A group that produced no expression occupies the same non-operand
			// slot as a bare directive, so it reconciles the connectors around it
			// the same way — without this, "a=1 or (sort=id:asc) or b=2" left the
			// second "or" unpaired and BuildE reported a dangling connector for
			// valid DSL (M12 in its parenthesized spelling).
			absorbAroundNonOperand(idx)
		}
		// Add operators
		if child.Operator == OperationAnd || child.Operator == OperationOr || child.Operator == OperationNot {
			items = append(items, child.Operator)
		}
	}

	if len(items) == 0 {
		return nil
	}

	// If we have only one item and it's an expression, return it
	if len(items) == 1 {
		if expr, ok := items[0].(Expr); ok {
			return expr
		}
		// A lone connector ("not", "and", "or" with nothing to join) is
		// dropped; without a diagnostic here it vanished silently.
		if op, ok := items[0].(Operation); ok {
			if op == OperationNot {
				addDiag(diags, "dangling 'not' with no operand dropped")
			} else {
				addDiag(diags, "dangling %q connector dropped", string(op))
			}
		}
		return nil
	}

	// Process with proper precedence: NOT > AND > OR
	return processWithPrecedence(items, diags)
}

// processWithPrecedence reduces the interleaved expression/operator sequence
// with NOT > AND > OR precedence. Every pass works POSITIONALLY on the item
// sequence itself — the previous implementation split items into parallel
// expressions[]/operators[] slices and paired them by index, which drifts one
// slot whenever a connector is implicit: "a=1 not b=2" negated a instead of b,
// and "a=1 b=2 or c=3" attached the OR to (a,b) instead of (b,c).
func processWithPrecedence(items []any, diags *[]error) Expr {
	if len(items) == 0 {
		return nil
	}

	// First pass: NOT (highest precedence, unary prefix). A NOT wraps the NEXT
	// expression in source order; consecutive NOTs stack ("not not a=1").
	// A trailing NOT with no operand is dropped (and diagnosed).
	resolved := make([]any, 0, len(items))
	pendingNots := 0
	for _, item := range items {
		switch v := item.(type) {
		case Operation:
			if v == OperationNot {
				pendingNots++
			} else {
				resolved = append(resolved, v)
			}
		case Expr:
			e := v
			for ; pendingNots > 0; pendingNots-- {
				e = NotExpr{Operands: []Expr{e}}
			}
			resolved = append(resolved, e)
		}
	}
	if pendingNots > 0 {
		addDiag(diags, "dangling 'not' with no operand dropped")
	}

	// Second pass: make implicit conjunction explicit. Two expressions
	// adjacent with no connector between them (e.g. "a=1 b=2", or a filter
	// following a load=/sort=/page= segment that emits no connector) are
	// AND-ed. Inserting the operator HERE — before the binary reduction
	// passes — gives the implicit form exactly the precedence of a written
	// "and": "a=1 b=2 or c=3" builds (a AND b) OR c, identical to
	// "a=1 and b=2 or c=3". (It used to be joined after OR reduction, which
	// silently made the implicit AND bind LOOSER than or.)
	withImplicit := make([]any, 0, len(resolved))
	for _, item := range resolved {
		if _, isExpr := item.(Expr); isExpr && len(withImplicit) > 0 {
			if _, prevIsExpr := withImplicit[len(withImplicit)-1].(Expr); prevIsExpr {
				withImplicit = append(withImplicit, OperationAnd)
			}
		}
		withImplicit = append(withImplicit, item)
	}
	resolved = withImplicit

	// Third and fourth passes: the binary connectors, in precedence order.
	resolved = reduceBinary(resolved, OperationAnd, diags)
	resolved = reduceBinary(resolved, OperationOr, diags)

	// Safety net: anything still left unpaired (dangling connectors are
	// dropped by reduceBinary) is combined with AND rather than silently
	// dropping all but the first expression.
	var expressions []Expr
	for _, item := range resolved {
		if e, ok := item.(Expr); ok {
			expressions = append(expressions, e)
		}
	}
	if len(expressions) == 0 {
		return nil
	}
	if len(expressions) == 1 {
		return expressions[0]
	}
	return AndExpr{Operands: expressions}
}

// reduceBinary collapses every run of one connector ("expr op expr op expr …")
// into a SINGLE n-ary AndExpr/OrExpr, preserving source positions. A dangling
// connector with no expression on one side (leading/trailing/doubled
// "and"/"or") is dropped (with a diagnostic) rather than pairing two unrelated
// expressions.
//
// Runs collapse flat rather than nesting left-associatively. Both connectors
// are associative, so the two shapes mean the same thing, but the old
// AndExpr{AndExpr{AndExpr{a,b},c},d} form was N levels deep for N conjuncts:
// every adapter recursed once per level and the SQL renderer re-joined and
// re-parenthesized the whole accumulated string at each one, so rendering a
// flat "a=1 and b=2 and …" chain was quadratic (8 000 conjuncts took 387ms,
// 50 000 took twelve seconds) and the SQL carried N redundant paren pairs.
func reduceBinary(items []any, op Operation, diags *[]error) []any {
	out := make([]any, 0, len(items))
	for i := 0; i < len(items); i++ {
		o, isOp := items[i].(Operation)
		if !isOp || o != op {
			out = append(out, items[i])
			continue
		}

		var left, right Expr
		var lok, rok bool
		if len(out) > 0 {
			left, lok = out[len(out)-1].(Expr)
		}
		if i+1 < len(items) {
			right, rok = items[i+1].(Expr)
		}
		if !lok || !rok {
			addDiag(diags, "dangling %q connector dropped", string(op))
			continue
		}

		operands := []Expr{left, right}
		i++ // the right operand is consumed
		// Absorb the rest of the run: "… op expr" pairs immediately following.
		for i+2 < len(items) {
			nextOp, isNextOp := items[i+1].(Operation)
			if !isNextOp || nextOp != op {
				break
			}
			nextExpr, ok := items[i+2].(Expr)
			if !ok {
				break
			}
			operands = append(operands, nextExpr)
			i += 2
		}

		if op == OperationAnd {
			out[len(out)-1] = AndExpr{Operands: operands}
		} else {
			out[len(out)-1] = OrExpr{Operands: operands}
		}
	}
	return out
}

func getFinalExpr(node Node) Expr {
	// If the node itself has expressions, return the last one
	if len(node.Expression) > 0 {
		return node.Expression[len(node.Expression)-1]
	}

	// If no children, return nil
	if len(node.Children) == 0 {
		return nil
	}

	// If only one child, return its expression
	if len(node.Children) == 1 {
		child := node.Children[0]
		if len(child.Expression) > 0 {
			// Return the last (most recent) expression from the child
			return child.Expression[len(child.Expression)-1]
		}
		return nil
	}

	// For multiple children, we need to build a proper logical expression tree
	// The expressionParser should have already built the logical expressions
	// We just need to find the final expression that represents the entire tree

	// Look for the most recent logical expression that combines all operands
	for i := len(node.Children) - 1; i >= 0; i-- {
		child := node.Children[i]

		// Skip child nodes (parentheses groups) as they should be processed separately
		if child.Operator == OperationChild {
			continue
		}

		// Look for logical operators that have expressions
		if (child.Operator == OperationAnd || child.Operator == OperationOr || child.Operator == OperationNot) &&
			len(child.Expression) > 0 {
			// Return the most recent expression from this logical operator
			return child.Expression[len(child.Expression)-1]
		}
	}

	// If no logical expressions found, return nil
	return nil
}

func (f *figo) AddFiltersFromString(input string) error {
	// An empty (or blank) input clears the stored DSL: the documented contract
	// is "replaces the existing DSL", and the previous silent no-op made it
	// impossible to ever clear filters through this API. Parse hooks are
	// skipped — there is nothing to parse.
	//
	// The cleared DSL's materialized state is dropped HERE rather than at the
	// next Build. Deferring it made the wipe retroactive: `builtFromDSL` is
	// instance-wide, so an AddFilter made AFTER the DSL was cleared — the case
	// BuildE's own comment and README Pattern A promise is kept — was wiped
	// along with the stale DSL clauses, and a per-request tenant scope added to
	// a cloned template silently vanished into an unfiltered scan.
	if strings.TrimSpace(input) == "" {
		f.mu.Lock()
		f.dsl = ""
		if f.builtFromDSL {
			f.clauses = []Expr{}
			f.clausesAsked = nil
			f.preloads = make(map[string][]Expr)
			if f.sortFromDSL {
				f.sort = nil
				f.sortFromDSL = false
			}
			f.resetDSLPage()
			f.builtFromDSL = false
		}
		f.mu.Unlock()
		return nil
	}

	// Snapshot the plugin manager under the lock; it may be swapped
	// concurrently by SetPluginManager/RegisterPlugin.
	f.mu.RLock()
	pm := f.pluginManager
	f.mu.RUnlock()

	// Execute BeforeParse plugin hooks. ExecuteBeforeParse already attributes
	// the failing plugin ("plugin X BeforeParse error: ..."), so the error is
	// returned as-is rather than wrapped into a doubled prefix.
	if pm != nil {
		var err error
		input, err = pm.ExecuteBeforeParse(f, input)
		if err != nil {
			return err
		}
	}

	// Update DSL string (replace existing) - protected by mutex. The previous
	// DSL is kept for rollback: AfterParse hooks (LimitsPlugin, Validation-
	// Plugin) inspect the committed DSL via Clone+Build, so the new value must
	// be visible while they run — but a rejected DSL must not stay armed for a
	// later Build when the caller ignores the returned error.
	f.mu.Lock()
	prevDSL := f.dsl
	f.dsl = input
	f.mu.Unlock()

	// Execute AfterParse plugin hooks (error returned unwrapped, as above).
	if pm != nil {
		// The rollback must also run when a hook PANICS. ExecuteAfterParse
		// deliberately runs every hook and joins their errors, so a panic in a
		// later hook (a ValidationRule handler with an unchecked type
		// assertion, say) unwound past the error path, threw away an earlier
		// plugin's rejection and left the rejected DSL armed for the next
		// Build — breaking the guide's invariant that "a rejected DSL can never
		// be built later". Roll back first, then let the panic continue.
		committed := false
		defer func() {
			if !committed {
				f.mu.Lock()
				f.dsl = prevDSL
				f.mu.Unlock()
			}
		}()
		if err := pm.ExecuteAfterParse(f, input); err != nil {
			return err
		}
		committed = true
	}

	return nil
}

// AddFilter appends a programmatic expression. Plugin expression filters
// (e.g. FieldsPlugin's ignore/whitelist pruning) apply here exactly as they
// do to DSL input — register them BEFORE adding filters.
func (f *figo) AddFilter(exp Expr) {
	f.mu.RLock()
	pm := f.pluginManager
	naming := f.namingFunc
	f.mu.RUnlock()

	// Field names are converted HERE, on the way in and exactly once, so one
	// statement can no longer contain two spellings of the same identifier:
	// the DSL converts at parse time (:1250) and the adapters then render every
	// field verbatim (hunt #1 deliberately removed GORM's render-time second
	// conversion), so a programmatic EqExpr{Field: "userName"} used to reach
	// SQL as "userName" while AddSelectFields("userName") reached it as
	// "user_name" — one statement filtering a column that does not exist.
	// Converting before the plugin filters run also matches the DSL path, where
	// FieldsPlugin sees already-converted names.
	exp = NormalizeExprFields(exp, naming)

	// Filters run outside the lock (see Build).
	if pm != nil {
		exp = pm.ExecuteExprFilters(f, exp)
	}
	if exp == nil {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.clauses = append(f.clauses, exp)
	if !f.inFinalize {
		f.clausesAsked = append(f.clausesAsked, exp)
	}
}

func (f *figo) AddSelectFields(fields ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, field := range fields {
		f.selectFields[field] = true
		if !f.inFinalize {
			f.selectFieldsAsked[field] = true
		}
	}
}

// SetSelectFields replaces the projection set (no arguments clears it, which
// restores "select every column"). AddSelectFields can only ever widen the
// projection, so a plugin enforcing field policy had no way to remove a column
// from it — FieldsPlugin uses this to prune ignored/non-whitelisted columns
// the way it prunes them from the filter and sort.
func (f *figo) SetSelectFields(fields ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.selectFields = make(map[string]bool, len(fields))
	for _, field := range fields {
		f.selectFields[field] = true
	}
	// A finalizer pruning the projection must not erase what the caller asked
	// for — the next Build re-derives the render from the request.
	if !f.inFinalize {
		f.selectFieldsAsked = make(map[string]bool, len(fields))
		for _, field := range fields {
			f.selectFieldsAsked[field] = true
		}
	}
}

func (f *figo) GetClauses() []Expr {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Return a DEEP copy. Copying only the outer slice left every node's
	// reference-typed fields aliasing the live instance, so a caller inspecting
	// the snapshot could reach through it and rewrite the query being rendered:
	// snap[0].(InExpr).Values[0] = 999 turned IN (1,2,3) into IN (999,2,3) on
	// the instance itself — a data race as well as a correctness hole, and the
	// exact independence Clone documents at README:746. Replacing a whole
	// element was already a no-op; only the partial aliasing was the trap.
	result := make([]Expr, len(f.clauses))
	for i, e := range f.clauses {
		result[i] = cloneExpr(e)
	}
	return result
}

func (f *figo) GetPreloads() map[string][]Expr {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Deep copy, for the reason spelled out in GetClauses: copying the outer
	// slice alone left each expression's Values/Operands aliasing the live
	// preload conditions.
	result := make(map[string][]Expr, len(f.preloads))
	for k, exprs := range f.preloads {
		cp := make([]Expr, len(exprs))
		for i, e := range exprs {
			cp[i] = cloneExpr(e)
		}
		result[k] = cp
	}
	return result
}

// ClausesForRender and PreloadsForRender are the READ-ONLY snapshots the
// in-tree adapters render from. They copy the outer slice/map — so a concurrent
// append cannot be observed mid-render — but do NOT deep-copy the nodes, which
// is what GetClauses/GetPreloads add for PUBLIC callers (a caller inspecting a
// snapshot could otherwise reach through it and rewrite the query being
// rendered). That guarantee costs O(total bound-value size) per call, and a
// render calls it more than once (the raw SELECT takes one snapshot for the
// WHERE and one inside buildOrderBy, the Mongo aggregate three): with a
// DSL-supplied 20k-element <in> list it turned a ~2us pipeline build into
// ~400us of pure copying, output byte-identical. A renderer only reads, so it
// must not pay for it.
//
// Deliberately NOT part of the Figo interface: an out-of-tree implementation
// stays compilable, and an adapter reaches these with a type assertion
// (`if r, ok := f.(interface{ ClausesForRender() []Expr }); ok`), falling back
// to GetClauses. Whatever is returned must be treated as immutable: mutating a
// node mutates the live instance. Use GetClauses/GetPreloads for anything else.
func (f *figo) ClausesForRender() []Expr {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make([]Expr, len(f.clauses))
	copy(result, f.clauses)
	return result
}

func (f *figo) PreloadsForRender() map[string][]Expr {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make(map[string][]Expr, len(f.preloads))
	for k, exprs := range f.preloads {
		cp := make([]Expr, len(exprs))
		copy(cp, exprs)
		result[k] = cp
	}
	return result
}

func (f *figo) GetPage() Page {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.page
}

func (f *figo) GetSort() *OrderBy {
	f.mu.RLock()
	defer f.mu.RUnlock()
	// Return a copy (as GetClauses/GetPreloads do): handing out the internal
	// pointer would let a caller mutate the sort columns while adapters render
	// on other goroutines.
	return cloneOrderBy(f.sort)
}

// SetSort replaces the sort specification programmatically (nil clears it).
// The spec is copied in, so the caller's OrderBy stays independent. Like a
// sort= directive, the value set here is subject to plugin finalizers (e.g.
// FieldsPlugin prunes forbidden columns on the next Build).
//
// The sort now belongs to the caller and survives Build, exactly as SetPage's
// value does. Build used to clear the sort unconditionally before re-parsing,
// so setting one before Build on an instance that also had a DSL silently did
// nothing. A sort= directive in the DSL still wins, matching page=.
func (f *figo) SetSort(sort *OrderBy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := cloneOrderBy(sort)
	if c != nil && f.namingFunc != nil {
		// Column names are converted on the way in, for the reason spelled out
		// in AddFilter: a sort= directive converts at :1023 and the adapters
		// render the stored name verbatim, so SetSort("userName") used to emit
		// ORDER BY "userName" next to a projection of "user_name".
		//
		// Conversion goes through normalizeFieldName — the SAME per-'.'-segment
		// helper AddFilter uses — because a dotted name is a QUALIFIED path.
		// Handing the whole string to the naming func folded the qualifier into
		// the column (SnakeCaseNaming turns '.' into '_'), so "orders.user_name"
		// became the nonexistent "orders_user_name" in ORDER BY while the WHERE
		// from the same string still said "orders"."user_name": two spellings of
		// one identifier in one statement, on already-snake input, and on
		// SQLite a silently unsorted (hence differently paginated) result.
		//
		// A name already present in the stored sort is kept as-is: a plugin
		// finalizer that PRUNES the sort (FieldsPlugin re-sets the survivors
		// through this same method) hands back columns that are already in
		// converted form, and a non-idempotent naming func would otherwise
		// convert them a second time on every Build ("t_id" -> "t_t_id").
		var existing map[string]bool
		if f.sort != nil {
			existing = make(map[string]bool, len(f.sort.Columns))
			for _, col := range f.sort.Columns {
				existing[col.Name] = true
			}
		}
		for i := range c.Columns {
			if !existing[c.Columns[i].Name] {
				c.Columns[i].Name = normalizeFieldName(c.Columns[i].Name, f.namingFunc)
			}
		}
	}
	f.sort = c
	f.sortFromDSL = false
}

func (f *figo) SetPage(skip, take int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.page.Skip = skip
	f.page.Take = take
	f.page.validate()
	// The page now belongs to the caller, not the DSL: it must survive a
	// DSL replacement (the DSL-origin flag would reset it on rebuild).
	f.pageFromDSL = 0
}

// SetPageString applies a "skip:N,take:N" string. Whatever parses is applied;
// anything that does not is DISCARDED SILENTLY — use SetPageStringE to find
// out. It is kept for compatibility only.
func (f *figo) SetPageString(v string) {
	_ = f.SetPageStringE(v)
}

// SetPageStringE is SetPageString with the input it could not use reported.
// Valid segments are still applied (the same "build what parsed, report the
// rest" contract as BuildE), so a non-nil error means the resulting Page does
// not express all of the input.
//
// It exists because SetPageString normalized garbage into a plausible-looking
// page and had no diagnostic channel at all: "garbage" and "skip:abc" both
// silently left the {0,20} default, an unknown key was ignored, and
// "skip:-4,take:-9" clamped to {0,0} — a Take of 0 meaning "no LIMIT", i.e.
// the opposite of the tiny page the caller asked for.
func (f *figo) SetPageStringE(v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if strings.TrimSpace(v) == "" {
		return nil
	}

	var errs []error
	applied := false

	for _, s := range strings.Split(v, ",") {
		if strings.TrimSpace(s) == "" {
			errs = append(errs, fmt.Errorf("empty page segment in %q (expected skip:N or take:N)", v))
			continue
		}
		pageSplit := strings.Split(s, ":")
		if len(pageSplit) != 2 {
			errs = append(errs, fmt.Errorf("malformed page segment %q (expected skip:N or take:N)", s))
			continue
		}

		field := strings.TrimSpace(pageSplit[0])
		value := strings.TrimSpace(pageSplit[1])

		if field != "skip" && field != "take" {
			errs = append(errs, fmt.Errorf("unknown page key %q (expected skip or take)", field))
			continue
		}

		parseInt, parsErr := strconv.ParseInt(value, 10, 64)
		if parsErr != nil {
			errs = append(errs, fmt.Errorf("invalid page value %q for %q (expected an integer)", value, field))
			continue
		}
		if parseInt < 0 {
			// validate() clamps a negative to 0. For take that silently turns
			// "give me fewer rows" into "no LIMIT at all", so say so.
			errs = append(errs, fmt.Errorf("negative page value %d for %q was clamped to 0", parseInt, field))
		}

		switch field {
		case "skip":
			f.page.Skip = int(parseInt)
		case "take":
			f.page.Take = int(parseInt)
		}
		f.page.validate()
		applied = true
	}

	if applied {
		// Caller-owned now — see SetPage.
		f.pageFromDSL = 0
	}
	return errors.Join(errs...)
}

func (f *figo) GetSelectFields() map[string]bool {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Return a copy to avoid race conditions
	result := make(map[string]bool)
	for k, v := range f.selectFields {
		result[k] = v
	}
	return result
}

// SetNamingFunc installs the field-name transformer applied to every field
// name the DSL produces. Use a built-in (SnakeCaseNaming, NoChangeNaming) or
// any custom func. Passing nil resets to the default (SnakeCaseNaming).
// Configure this before Build()/AddFiltersFromString.
func (f *figo) SetNamingFunc(fn NamingFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fn == nil {
		fn = SnakeCaseNaming
	}
	f.namingFunc = fn
}

// GetNamingFunc returns the active field-name transformer (never nil).
func (f *figo) GetNamingFunc() NamingFunc {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.namingFunc
}

func (f *figo) SetAdapterObject(adapter Adapter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adapterObj = adapter
}

func (f *figo) GetAdapterObject() Adapter {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.adapterObj
}

// GetSqlString returns a SQL string based on the selected adapter.
// For AdapterGorm, ctx should be a *gorm.DB configured with Model(...).
// For AdapterRaw, ctx can be a table name (string) or RawContext.
//
// Registered plugins' BeforeQuery hooks run before the adapter renders (an
// error vetoes the render and returns ""); AfterQuery hooks run on the
// rendered SQL (an error vetoes the result the same way). Hooks must not
// render through this instance themselves — that would recurse.
func (f *figo) GetSqlString(ctx any, conditionType ...string) string {
	// Snapshot the adapter and plugin manager under the lock, then call them
	// WITHOUT holding it (the adapter and hooks call back into locked getters
	// like GetClauses).
	f.mu.RLock()
	adapter := f.adapterObj
	pm := f.pluginManager
	f.mu.RUnlock()

	if pm != nil {
		if err := pm.ExecuteBeforeQuery(f, ctx); err != nil {
			return ""
		}
	}
	if adapter != nil {
		if sql, ok := adapter.GetSqlString(f, ctx, conditionType...); ok {
			if pm != nil {
				if err := pm.ExecuteAfterQuery(f, ctx, sql); err != nil {
					return ""
				}
			}
			return sql
		}
	}
	return ""
}

// GetQuery delegates to the configured adapter to obtain a typed query object.
// Plugin BeforeQuery/AfterQuery hooks run around the render exactly as in
// GetSqlString (an error from either vetoes the result: nil is returned).
//
// When the configured ADAPTER cannot render, the result is never an untyped
// nil: the documented retrieval pattern asserts on the concrete type
// (`f.GetQuery(ctx).(figo.SQLQuery)`), and a type assertion on a nil interface
// PANICS. That turned an adapter-side render refusal — reachable straight from
// untrusted DSL now that the SQL adapters validate identifiers, e.g.
// "?filter=a%00b=1" — into a crashed request goroutine, with BuildE reporting
// nothing. A refusal is answered with a fail-closed Query instead: the value the
// adapter supplied if it supplied one (an adapter reporting ok=false must return
// either nil or a fail-closed value, never a partial render), otherwise
// failClosedQuery, whose SQL cannot select a row in any position.
//
// Two cases deliberately keep returning nil, because neither is reachable from
// query input and in-tree callers signal on them: a hook VETO (the plugin
// author's explicit decision, and the documented BeforeQuery/AfterQuery
// contract), and no adapter configured at all (a programming error — the same
// answer GetSqlString's "" gives).
func (f *figo) GetQuery(ctx any, conditionType ...string) Query {
	f.mu.RLock()
	adapter := f.adapterObj
	pm := f.pluginManager
	f.mu.RUnlock()

	if pm != nil {
		if err := pm.ExecuteBeforeQuery(f, ctx); err != nil {
			return nil
		}
	}
	if adapter != nil {
		q, ok := adapter.GetQuery(f, ctx, conditionType...)
		if ok {
			if pm != nil {
				if err := pm.ExecuteAfterQuery(f, ctx, q); err != nil {
					return nil
				}
			}
			return q
		}
		if q != nil {
			return q
		}
		return failClosedQuery()
	}
	return nil
}

// failClosedSQL is the SQL text of the fail-closed Query GetQuery hands back
// when the adapter refused to render. "1=0" cannot return a row in any of the
// three positions callers use a rendered query in: executed on its own it is a
// syntax error, appended after a WHERE keyword it matches nothing, and
// concatenated in place of a whole WHERE clause it is a syntax error again. An
// empty string would be silently match-EVERYTHING in the last of those.
const failClosedSQL = "1=0"

func failClosedQuery() Query { return SQLQuery{SQL: failClosedSQL} }

// GetDSL returns the current DSL string
func (f *figo) GetDSL() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.dsl
}

// Build parses the DSL into the internal clause tree. The adapter is optional
// here: pass it to Build(GormAdapter{}) to set (or override) the adapter that
// GetSqlString/GetQuery will use, or set it earlier via New / SetAdapterObject.
// Malformed DSL constructs are skipped silently; BuildE is the same operation
// with those skips reported.
func (f *figo) Build(adapter Adapter) {
	_ = f.BuildE(adapter)
}

// BuildE parses exactly like Build and additionally reports everything the
// parser had to drop: unrecognized tokens, operators with no field, invalid
// operator values (e.g. a <bet> range without ".."), and malformed
// sort=/page=/load= directives. The clause tree is still built from whatever
// DID parse — identical to Build — so a non-nil error means the built query
// does not express all of the input. Callers validating user-supplied DSL
// should treat a non-nil error as a rejection rather than running the
// (broader) query that remains.
func (f *figo) BuildE(adapter Adapter) error {
	f.mu.Lock()

	// A non-nil adapter selects/replaces the adapter; passing nil rebuilds
	// against whatever adapter was set previously (via an earlier Build or
	// SetAdapterObject).
	if adapter != nil {
		f.adapterObj = adapter
	}

	if f.dsl == "" {
		// If a previous Build materialized clause state FROM a DSL that has
		// since been cleared (AddFiltersFromString("")), that state must not
		// survive this rebuild. Programmatic state — AddFilter clauses added
		// with no DSL, SetPage, SetSort — is kept.
		if f.builtFromDSL {
			f.clauses = []Expr{}
			f.clausesAsked = nil
			f.preloads = make(map[string][]Expr)
			if f.sortFromDSL {
				f.sort = nil
				f.sortFromDSL = false
			}
			f.resetDSLPage()
			f.builtFromDSL = false
		}
		// Re-derive this render from the caller's clause list, so a previous
		// Build's finalizer output (notably the never-true pill) is not the
		// input to this one. The DSL path below gets this for free by
		// re-parsing; this is the only path where the pill could survive.
		f.clauses = append([]Expr(nil), f.clausesAsked...)
		f.mu.Unlock()
		// Even with no DSL, clause finalizers must run (a ScopePlugin's
		// mandatory filter applies to unfiltered queries too).
		defer f.guardPluginPanic()
		f.finalizeClauses()
		return nil
	}

	// Clear all DSL-derived state before rebuilding so Build is idempotent:
	// preloads used to accumulate (Build();Build() duplicated every load=
	// condition) and a previous DSL's sort survived a rebuild. The page and
	// sort are reset only when they came from a page=/sort= directive —
	// SetPage's and SetSort's effects must outlive Build.
	f.clauses = []Expr{}
	f.preloads = make(map[string][]Expr)
	if f.sortFromDSL {
		f.sort = nil
		f.sortFromDSL = false
	}
	f.resetDSLPage()
	f.builtFromDSL = true

	var diags []error
	var finalExpr Expr
	guardTripped := false
	if reason := dslResourceGuard(f.dsl); reason != "" {
		// Refused unscanned, and fail closed: the pill renders 1=0 on every
		// adapter, so an input too large to parse can only ever narrow the
		// query, and the non-nil BuildE error tells a validating caller to
		// reject it.
		addDiag(&diags, "%s; the input was not parsed and the query matches nothing", reason)
		finalExpr = OrExpr{}
		guardTripped = true
	} else {
		root := f.parseDSL(f.dsl, &diags)
		expressionParser(root, &diags)
		finalExpr = getFinalExpr(*root)
	}

	// Detach the freshly parsed preloads so plugin filters can run on them
	// outside the lock (concurrent readers see an empty map until the
	// filtered results are written back below).
	preloads := f.preloads
	f.preloads = make(map[string][]Expr)
	pm := f.pluginManager
	f.mu.Unlock()

	// An empty manager dispatches nothing, so it must not take the plugin path
	// at all: GetPluginManager creates the manager on first call, and a getter
	// must not change what the next Build produces.
	if !pm.hasPlugins() {
		pm = nil
	}

	// From here on user plugin code runs with the instance state already
	// cleared — a panicking hook must leave it fail-closed, not filter-less.
	defer f.guardPluginPanic()

	// Run plugin expression filters (e.g. FieldsPlugin's ignore/whitelist
	// pruning) OUTSIDE the lock — a FilterExpr callback may call back into
	// this instance's read methods, which must not deadlock. Preload
	// conditions parse through the same DSL, so they are filtered as well.
	if pm != nil {
		// The resource-guard pill must skip the filter pass: pruning rebuilds
		// logical nodes and drops an empty OrExpr entirely (pruneExprFields),
		// which would launder the refusal into a filter-less match-everything
		// query the moment a FieldsPlugin is registered.
		if finalExpr != nil && !guardTripped {
			finalExpr = pm.ExecuteExprFilters(f, finalExpr)
		}
		for table, exprs := range preloads {
			// An unconditioned preload (registered with no filter) has nothing
			// to prune and must survive; only a preload whose EVERY condition
			// was pruned is dropped (fail closed — pruning must not widen it).
			if len(exprs) == 0 {
				continue
			}
			kept := exprs[:0]
			for _, e := range exprs {
				if pruned := pm.ExecuteExprFilters(f, e); pruned != nil {
					kept = append(kept, pruned)
				}
			}
			if len(kept) == 0 {
				delete(preloads, table)
			} else {
				preloads[table] = kept
			}
		}
		// Preload finalizers run after pruning, so a mandatory condition a
		// plugin adds here cannot be pruned away by the same Build. Relations
		// are visited in sorted order to keep the dispatch deterministic.
		rels := make([]string, 0, len(preloads))
		for table := range preloads {
			rels = append(rels, table)
		}
		sort.Strings(rels)
		for _, table := range rels {
			before := preloads[table]
			final := pm.ExecutePreloadFinalizers(f, table, before)
			// Only a finalizer that actually EMPTIED a non-empty list drops the
			// relation. An unconditioned preload ("load=[Orders:]", or a segment
			// whose filter yielded no conditions) legitimately carries zero
			// conditions — the pruning loop above skips it for exactly that
			// reason (:1105-1113) — so treating "came back empty" as "drop it"
			// deleted it merely because a plugin manager existed: registering ANY
			// plugin, even one implementing no finalizer at all, silently stopped
			// preloading the relation on every adapter with no diagnostic.
			if len(final) == 0 && len(before) > 0 {
				delete(preloads, table)
			} else {
				preloads[table] = final
			}
		}
	}

	f.mu.Lock()
	if finalExpr != nil {
		f.clauses = append(f.clauses, finalExpr)
	}
	// The freshly parsed tree, plus any programmatic clauses that survived it,
	// is what the caller asked for; finalizer output must not join it.
	f.clausesAsked = append([]Expr(nil), f.clauses...)
	f.preloads = preloads
	f.mu.Unlock()

	f.finalizeClauses()

	return errors.Join(diags...)
}

// guardPluginPanic is deferred around plugin hook execution in BuildE: Build
// clears the instance state BEFORE the fallible hooks run, so a caller that
// recovers a plugin panic (e.g. HTTP middleware) would otherwise keep an
// instance that renders a filter-less match-everything query — without even a
// ScopePlugin's tenant clause. Instead the clause list is replaced with the
// canonical never-true clause (an empty OrExpr renders 1=0) and the panic is
// re-raised. Must be deferred only while f.mu is NOT held.
func (f *figo) guardPluginPanic() {
	if r := recover(); r != nil {
		f.mu.Lock()
		f.clauses = []Expr{OrExpr{}}
		f.preloads = make(map[string][]Expr)
		// Per-Build finalizer state must not survive the panic either: a stuck
		// inFinalize silently ignored every later SetSelectFields, so the caller
		// could not narrow the projection after recovering (see finalizeClauses,
		// which also unwinds it — this is the belt to that braces, and covers a
		// panic raised anywhere else under the guard).
		f.inFinalize = false
		f.mu.Unlock()
		panic(r)
	}
}

// finalizeClauses runs registered ClauseFinalizer plugins over the top-level
// clause list and writes the result back. Runs outside the lock (a finalizer
// may call back into read methods).
func (f *figo) finalizeClauses() {
	f.mu.RLock()
	pm := f.pluginManager
	f.mu.RUnlock()
	// An empty manager has no finalizer to run, and must leave no trace: see
	// hasPlugins (GetPluginManager creates the manager lazily, and a getter must
	// not change what the next Build produces).
	if !pm.hasPlugins() {
		return
	}

	f.mu.Lock()
	sortOrigin := f.sortFromDSL
	// Re-derive this render's projection from the caller's request, so a
	// finalizer's pruning applies to this Build only and never accumulates
	// across rebuilds (it used to be sticky: widening the policy and
	// rebuilding still yielded the previously narrowed projection).
	f.selectFields = make(map[string]bool, len(f.selectFieldsAsked))
	for k, v := range f.selectFieldsAsked {
		f.selectFields[k] = v
	}
	f.inFinalize = true
	f.mu.Unlock()

	// Both fields below are per-CALL state, so they unwind with a panic instead
	// of being restored on the normal return path only. A finalizer that panics
	// and an application that recovers it — the case guardPluginPanic exists for
	// — used to strand them: inFinalize stuck ON made every later
	// SetSelectFields/AddSelectFields write the render but not the request, so
	// the caller could no longer change the projection AT ALL (narrowing it to
	// drop "password" was silently ignored and the wide projection kept
	// rendering), and sortFromDSL stuck OFF left a DSL-derived sort marked
	// caller-owned, so the previous DSL's sort= leaked into a later query that
	// named no sort — exactly what the restore below exists to prevent.
	defer func() {
		f.mu.Lock()
		f.inFinalize = false
		// A finalizer that rewrites the sort (FieldsPlugin prunes forbidden
		// columns) goes through the public SetSort, which marks the sort
		// caller-owned. Restore where the sort actually came from: pruning a
		// DSL-derived sort leaves it DSL-derived, so the next rebuild still
		// clears it instead of leaking it into a later query.
		f.sortFromDSL = sortOrigin
		f.mu.Unlock()
	}()

	finalized := pm.ExecuteClauseFinalizers(f, f.GetClauses())

	f.mu.Lock()
	f.clauses = finalized
	f.mu.Unlock()
}

// exprField returns the field a leaf expression filters on, or "" for
// expressions without one (OrderBy, unknown types).
func exprField(expr Expr) string {
	switch e := expr.(type) {
	case EqExpr:
		return e.Field
	case GteExpr:
		return e.Field
	case GtExpr:
		return e.Field
	case LtExpr:
		return e.Field
	case LteExpr:
		return e.Field
	case NeqExpr:
		return e.Field
	case LikeExpr:
		return e.Field
	case ILikeExpr:
		return e.Field
	case RegexExpr:
		return e.Field
	case InExpr:
		return e.Field
	case NotInExpr:
		return e.Field
	case BetweenExpr:
		return e.Field
	case IsNullExpr:
		return e.Field
	case NotNullExpr:
		return e.Field
	case JsonPathExpr:
		return e.Field
	case ArrayContainsExpr:
		return e.Field
	case ArrayOverlapsExpr:
		return e.Field
	case FullTextSearchExpr:
		return e.Field
	case GeoDistanceExpr:
		return e.Field
	case CustomExpr:
		return e.Field
	default:
		return ""
	}
}

// PruneExprFields removes every leaf whose field fails keep, rebuilding the
// logical structure around the survivors. Dropping a leaf drops it from its
// parent's operand list, so no dangling AND/OR/NOT is left behind. Used by
// the plugins package (FieldsPlugin) and available for custom ExprFilters.
func PruneExprFields(expr Expr, keep func(field string) bool) Expr {
	return pruneExprFields(expr, keep)
}

// ExprField returns the field a leaf expression filters on, or "" for
// expressions without one (logical nodes, OrderBy, unknown types). It works
// on the value-typed nodes returned by GetClauses; for the pointer nodes a
// Walk visitor receives, use NodeField.
func ExprField(e Expr) string {
	return exprField(e)
}

// CloneExpr returns an independent deep copy of an expression tree.
func CloneExpr(e Expr) Expr {
	return cloneExpr(e)
}

// NormalizeExprFields returns a copy of e with naming applied to the field
// name of every leaf node (and to every OrderBy column), rebuilding the
// logical structure around them. A nil naming func returns e unchanged.
//
// figo converts field names exactly ONCE, on the way in: the DSL parser does
// it while parsing and AddFilter/SetSort do it for programmatic input, so
// every adapter can render the stored name verbatim. Use this helper for any
// expression that reaches an instance by another route — most notably a
// plugin that INJECTS a clause from FinalizeClauses (a mandatory tenant scope,
// say), which bypasses AddFilter and would otherwise contribute the one field
// in the statement that is spelled differently from all the others.
//
// Values are shared with the input (only field names and the tree structure
// are rebuilt); use CloneExpr first if you need full independence.
func NormalizeExprFields(e Expr, naming func(string) string) Expr {
	if naming == nil || e == nil {
		return e
	}
	return normalizeExprFields(e, naming)
}

func normalizeExprFields(e Expr, naming func(string) string) Expr {
	switch v := e.(type) {
	case AndExpr:
		return AndExpr{Operands: normalizeOperands(v.Operands, naming)}
	case OrExpr:
		return OrExpr{Operands: normalizeOperands(v.Operands, naming)}
	case NotExpr:
		return NotExpr{Operands: normalizeOperands(v.Operands, naming)}
	case OrderBy:
		cols := make([]OrderByColumn, len(v.Columns))
		copy(cols, v.Columns)
		for i := range cols {
			cols[i].Name = normalizeFieldName(cols[i].Name, naming)
		}
		return OrderBy{Columns: cols}

	// Leaf nodes — enumerated explicitly (the same list exprField carries) so
	// a future Expr type with a Field is caught here rather than silently
	// keeping the caller's spelling.
	case EqExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case GteExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case GtExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case LtExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case LteExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case NeqExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case LikeExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case ILikeExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case RegexExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case InExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case NotInExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case BetweenExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case IsNullExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case NotNullExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case JsonPathExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case ArrayContainsExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case ArrayOverlapsExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case FullTextSearchExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case GeoDistanceExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	case CustomExpr:
		v.Field = normalizeFieldName(v.Field, naming)
		return v
	default:
		// Unknown type: pass through rather than dropping it.
		return e
	}
}

// normalizeFieldName applies naming to each '.'-separated segment of a field
// name. A dotted name is a QUALIFIED path ("users.first_name", or a Mongo
// embedded-document path) and the SQL adapters quote it segment by segment, so
// converting the whole string at once would fold the qualifier into the column
// (SnakeCaseNaming turns '.' into '_': "users_first_name", a column that does
// not exist) and break the documented Walk/SetNodeField qualification recipe.
func normalizeFieldName(field string, naming func(string) string) string {
	if field == "" {
		return field
	}
	if !strings.Contains(field, ".") {
		return naming(field)
	}
	parts := strings.Split(field, ".")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = naming(p)
	}
	return strings.Join(parts, ".")
}

func normalizeOperands(operands []Expr, naming func(string) string) []Expr {
	if operands == nil {
		return nil
	}
	out := make([]Expr, len(operands))
	for i, o := range operands {
		out[i] = normalizeExprFields(o, naming)
	}
	return out
}

// pruneExprFields removes every leaf whose field fails keep, rebuilding the
// logical structure around the survivors. Dropping a leaf drops it from its
// parent's operand list, so no dangling AND/OR/NOT is left behind.
func pruneExprFields(expr Expr, keep func(field string) bool) Expr {
	switch e := expr.(type) {
	case AndExpr:
		operands := pruneOperands(e.Operands, keep)
		if len(operands) == 0 {
			return nil
		}
		if len(operands) == 1 {
			return operands[0]
		}
		return AndExpr{Operands: operands}
	case OrExpr:
		operands := pruneOperands(e.Operands, keep)
		if len(operands) == 0 {
			return nil
		}
		if len(operands) == 1 {
			return operands[0]
		}
		return OrExpr{Operands: operands}
	case NotExpr:
		operands := pruneOperands(e.Operands, keep)
		if len(operands) == 0 {
			return nil
		}
		return NotExpr{Operands: operands}
	default:
		if field := exprField(e); field != "" && !keep(field) {
			return nil
		}
		return e
	}
}

func pruneOperands(operands []Expr, keep func(field string) bool) []Expr {
	var kept []Expr
	for _, operand := range operands {
		if pruned := pruneExprFields(operand, keep); pruned != nil {
			kept = append(kept, pruned)
		}
	}
	return kept
}

// Plugin Management Methods

// SetPluginManager sets the plugin manager
func (f *figo) SetPluginManager(manager *PluginManager) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pluginManager = manager
}

// GetPluginManager returns the current plugin manager, creating it on first
// use. It used to return a nil *PluginManager on any instance that had never
// registered a plugin, and PluginManager's methods are not nil-receiver-safe,
// so every call the docs show on the result (ListPlugins, GetPlugin, the
// Execute* helpers) panicked on a fresh instance. Constructing it lazily here
// mirrors what RegisterPlugin already does.
func (f *figo) GetPluginManager() *PluginManager {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pluginManager == nil {
		f.pluginManager = NewPluginManager()
	}
	return f.pluginManager
}

// RegisterPlugin registers a plugin
func (f *figo) RegisterPlugin(plugin Plugin) error {
	// Lazily create the manager under the lock so two concurrent first-calls
	// can't each construct one (with one silently lost). The manager has its own
	// lock, so call into it without holding f.mu.
	f.mu.Lock()
	if f.pluginManager == nil {
		f.pluginManager = NewPluginManager()
	}
	pm := f.pluginManager
	f.mu.Unlock()

	if err := pm.RegisterPlugin(plugin); err != nil {
		return err
	}

	// Initialize the plugin; roll back the registration on failure so a
	// broken plugin's hooks never run and a fixed one can re-register.
	if err := plugin.Initialize(f); err != nil {
		_ = pm.UnregisterPlugin(plugin.Name())
		return err
	}
	return nil
}

// UnregisterPlugin removes a plugin
func (f *figo) UnregisterPlugin(name string) error {
	f.mu.RLock()
	pm := f.pluginManager
	f.mu.RUnlock()
	if pm == nil {
		return fmt.Errorf("no plugin manager available")
	}
	return pm.UnregisterPlugin(name)
}
