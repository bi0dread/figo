package figo

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gobeam/stringy"
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
	// stringy drops leading underscores ("_id" -> "id"), which would silently
	// break fields like Mongo's canonical _id — split them off and restore
	// them after conversion.
	trimmed := strings.TrimLeft(field, "_")
	prefix := field[:len(field)-len(trimmed)]
	// Use stringy to convert to snake_case, but handle edge cases: if stringy
	// returns an empty string, fall back to the original name rather than
	// silently dropping the column.
	result := stringy.New(trimmed).SnakeCase("?", "").ToLower()
	if result == "" {
		return field
	}
	return prefix + result
}

// NoChangeNaming leaves field names exactly as written in the DSL.
var NoChangeNaming NamingFunc = func(field string) string { return field }

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

// AdapterType removed: adapters are selected via Adapter objects

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
	AfterQuery(f Figo, ctx any, result interface{}) error
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

// PluginManager manages plugins
type PluginManager struct {
	plugins map[string]Plugin
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
	return nil
}

// GetPlugin retrieves a plugin by name
func (pm *PluginManager) GetPlugin(name string) (Plugin, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	plugin, exists := pm.plugins[name]
	return plugin, exists
}

// ListPlugins returns all registered plugins
func (pm *PluginManager) ListPlugins() []Plugin {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	plugins := make([]Plugin, 0, len(pm.plugins))
	for _, plugin := range pm.plugins {
		plugins = append(plugins, plugin)
	}
	return plugins
}

// Hooks run on a snapshot taken under the lock, with the lock released while
// user plugin code executes — a hook that calls back into the manager
// (Register/Unregister/List) must not deadlock.

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
func (pm *PluginManager) ExecuteAfterQuery(f Figo, ctx any, result interface{}) error {
	for _, plugin := range pm.ListPlugins() {
		if err := plugin.AfterQuery(f, ctx, result); err != nil {
			return fmt.Errorf("plugin %s AfterQuery error: %w", plugin.Name(), err)
		}
	}
	return nil
}

// ExecuteBeforeParse executes all plugins' BeforeParse hooks
func (pm *PluginManager) ExecuteBeforeParse(f Figo, dsl string) (string, error) {
	modifiedDSL := dsl
	for _, plugin := range pm.ListPlugins() {
		var err error
		modifiedDSL, err = plugin.BeforeParse(f, modifiedDSL)
		if err != nil {
			return "", fmt.Errorf("plugin %s BeforeParse error: %w", plugin.Name(), err)
		}
	}
	return modifiedDSL, nil
}

// ExecuteAfterParse executes all plugins' AfterParse hooks
func (pm *PluginManager) ExecuteAfterParse(f Figo, dsl string) error {
	for _, plugin := range pm.ListPlugins() {
		if err := plugin.AfterParse(f, dsl); err != nil {
			return fmt.Errorf("plugin %s AfterParse error: %w", plugin.Name(), err)
		}
	}
	return nil
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

// Comparison expressions
type EqExpr struct {
	Field string
	Value any
}
type GteExpr struct {
	Field string
	Value any
}
type GtExpr struct {
	Field string
	Value any
}
type LtExpr struct {
	Field string
	Value any
}
type LteExpr struct {
	Field string
	Value any
}
type NeqExpr struct {
	Field string
	Value any
}
type LikeExpr struct {
	Field string
	Value any
}

// Regex expression
type RegexExpr struct {
	Field string
	Value any
}

// Logical expressions
type AndExpr struct{ Operands []Expr }
type OrExpr struct{ Operands []Expr }

// NotExpr matches when NONE of its operands match: NOT(a OR b). All adapters
// implement this reading (SQL "NOT (a OR b)", Mongo $nor, GORM clause.Not,
// Elasticsearch must_not). The DSL parser only emits single-operand NotExpr;
// multi-operand forms come from AddFilter.
type NotExpr struct{ Operands []Expr }

// Sorting expressions
type OrderByColumn struct {
	Name string
	Desc bool
}

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

type InExpr struct {
	Field  string
	Values []any
}

type NotInExpr struct {
	Field  string
	Values []any
}

type BetweenExpr struct {
	Field string
	Low   any
	High  any
}

type IsNullExpr struct{ Field string }

type NotNullExpr struct{ Field string }

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
// invoke Handler with the Field exactly as held here (unquoted, not renamed),
// the Operator and the Value; it returns a SQL fragment with '?' placeholders
// plus its bind args. The handler owns identifier quoting/naming. The MongoDB
// and Elasticsearch adapters reject CustomExpr — its output is a SQL fragment.
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

type Figo interface {
	AddFiltersFromString(input string) error
	AddFilter(exp Expr)
	AddSelectFields(fields ...string)
	GetDSL() string
	SetPluginManager(manager *PluginManager)
	GetPluginManager() *PluginManager
	RegisterPlugin(plugin Plugin) error
	UnregisterPlugin(name string) error
	SetNamingFunc(fn NamingFunc)
	GetNamingFunc() NamingFunc
	SetPage(skip, take int)
	SetPageString(v string)
	SetAdapterObject(adapter Adapter)
	GetSelectFields() map[string]bool
	GetClauses() []Expr
	GetPreloads() map[string][]Expr
	GetPage() Page
	GetSort() *OrderBy
	GetAdapterObject() Adapter
	GetSqlString(ctx any, conditionType ...string) string
	GetQuery(ctx any, conditionType ...string) Query
	Build(adapter Adapter)
	BuildE(adapter Adapter) error
	Explain() string
	Clone() Figo
	Walk(visit func(Expr))
}

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
	mu            sync.RWMutex // Mutex for concurrent access protection
}

// New constructs a new instance. Supply the adapter when you build:
// Build(GormAdapter{}) (or via SetAdapterObject). New itself takes no adapter.
func New() Figo {
	return &figo{page: Page{
		Skip: 0,
		Take: 20,
	}, preloads: make(map[string][]Expr), selectFields: make(map[string]bool), clauses: make([]Expr, 0), namingFunc: SnakeCaseNaming}
}

func (p *Page) validate() {
	if p.Skip < 0 {
		p.Skip = 0
	}
	if p.Take < 0 {
		p.Take = 0
	}
}

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

// parseDSL scans the DSL into the Node tree. Malformed constructs are skipped
// (never fatal) exactly as they always were; each skip is additionally
// recorded into diags so BuildE can report what the built query does NOT
// include.
func (f *figo) parseDSL(expr string, diags *[]error) *Node {
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
					// A closing quote ends the token only outside a bracketed list.
					// Inside a list (e.g. <in>["a,b","c"]) more quoted elements can
					// follow, so keep scanning and reset the quote state.
					if bracketDepth > 0 {
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
					parenDepth++
					j++
					continue
				}
				if expr[j] == ')' {
					if parenDepth > 0 {
						parenDepth--
						j++
						continue
					}
					break
				}

				j++
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
					k := j - 1
					if strings.HasPrefix(token, string(OperationLoad)+"=") {
						bracketCount := 1
						for k < len(expr) && bracketCount > 0 {

							switch expr[k] {
							case '[':
								bracketCount++
							case ']':
								bracketCount--
							}
							k++

						}
						//k++

						loadLabel := fmt.Sprintf("%v=[", string(OperationLoad))

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

						loadSplit := strings.Split(content, "|")
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
							loadContent := strings.TrimSpace(l[colonIndex+1:])

							loadRootNode := f.parseDSL(loadContent, diags)
							expressionParser(loadRootNode)
							loadExpr := getFinalExpr(*loadRootNode)
							if loadExpr != nil {
								f.preloads[table] = append(f.preloads[table], loadExpr)
							}

						}
						i = k
						continue

					} else if strings.HasPrefix(token, string(OperationPage)+"=") {

						pageLabel := fmt.Sprintf("%v=", string(OperationPage))
						content := token[strings.Index(token, pageLabel)+len(pageLabel):]

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
								case "take":
									f.page.Take = int(parseInt)
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

							if dir := strings.ToLower(value); dir != "asc" && dir != "desc" {
								addDiag(diags, "invalid sort direction %q for field %q (expected asc or desc)", value, field)
							}
							c = append(c, OrderByColumn{
								Name: f.parsFieldsName(field),
								Desc: strings.ToLower(value) == "desc",
							})

						}

						sortExpr := OrderBy{
							Columns: c,
						}
						f.sort = &sortExpr
						// Advance past the whole sort token (see the page branch).
						i = j
						continue

					}

					i = k
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
							nextToken := strings.TrimSpace(expr[nextStart:nextEnd])
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
												// Inside a bracketed list more quoted
												// elements may follow (<in>["a b","c"]).
												if bracketCount > 0 {
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
						i = j
						continue
					}
					if field == "" {
						addDiag(diags, "operator %q with no field name (token %q)", operator, combinedToken)
					}

					// Ignore-field filtering happens on the built expression
					// tree in Build (pruning a condition there cannot leave an
					// orphaned and/or/not behind, unlike skipping the node here).

					// Pass the raw literal (quotes intact): getClausesFromOperation
					// types each literal exactly once. Parsing here and re-parsing
					// there through a %v round-trip destroyed quoted-string typing
					// ("0123" became int64 123), nulls and dates.
					convertedField := f.parsFieldsName(field)
					newNode := &Node{Operator: operator, Value: valueStr, Field: convertedField, Parent: current, Expression: make([]Expr, 0)}
					clauseExpr := getClausesFromOperation(operator, convertedField, valueStr)
					if clauseExpr == nil {
						addDiag(diags, "invalid value %q for operator %q on field %q", valueStr, operator, field)
					}
					newNode.Expression = append(newNode.Expression, clauseExpr)
					current.Children = append(current.Children, newNode)
					i = j
				}
			} else {
				i = j
			}
		}
	}

	return root
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

// parseDate attempts to parse a string as a date using common formats
func parseDate(s string) (time.Time, error) {
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
		if f64, err := strconv.ParseFloat(s, 64); err == nil {
			return f64
		}
		if dateVal, err := parseDate(s); err == nil {
			return dateVal
		}
	}
	return s
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
func parseListLiteral(raw string) []any {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
	}
	if s == "" {
		return nil
	}
	// Split on commas OUTSIDE quotes so quoted elements can contain commas.
	parts := splitOutsideQuotes(s, ',')
	vals := make([]any, 0, len(parts))
	for _, p := range parts {
		vals = append(vals, parseScalarLiteral(p))
	}
	return vals
}

func getClausesFromOperation(o Operation, field string, value any) Expr {
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
			return parseListLiteral(rawStr)
		}
		if vals, ok := value.([]any); ok {
			return vals
		}
		return parseListLiteral(fmt.Sprintf("%v", value))
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
		if idx := strings.Index(s, ".."); idx > 0 {
			low := strings.TrimSpace(s[:idx])
			high := strings.TrimSpace(s[idx+2:])
			return BetweenExpr{Field: field, Low: parseScalarLiteral(low), High: parseScalarLiteral(high)}
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

func expressionParser(node *Node) {
	// First, recursively process all child nodes to build their expressions
	for _, child := range node.Children {
		if child.Operator == OperationChild {
			expressionParser(child)
		}
	}

	if node.Operator == OperationChild {
		if len(node.Children) == 1 {
			node.Expression = append(node.Expression, node.Children[0].Expression...)
			return
		}

		// For multiple children, build proper logical expression tree with precedence
		expr := buildExpressionTreeWithPrecedence(node.Children)
		if expr != nil {
			node.Expression = append(node.Expression, expr)
		}
	} else {
		// For non-child nodes, recursively process children
		for _, child := range node.Children {
			if child.Operator == OperationChild {
				expressionParser(child)
			}
		}

		// After processing children, build expression tree with precedence
		expr := buildExpressionTreeWithPrecedence(node.Children)
		if expr != nil {
			node.Expression = append(node.Expression, expr)
		}
	}
}

// buildExpressionTreeWithPrecedence builds a proper expression tree respecting operator precedence
func buildExpressionTreeWithPrecedence(children []*Node) Expr {
	if len(children) == 0 {
		return nil
	}

	// Build a list of expressions and operators in order
	var items []interface{} // Can be Expr or Operation

	for _, child := range children {
		// Add expressions from this child
		if len(child.Expression) > 0 {
			items = append(items, child.Expression[len(child.Expression)-1])
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
		return nil
	}

	// Process with proper precedence: NOT > AND > OR
	return processWithPrecedence(items)
}

// processWithPrecedence reduces the interleaved expression/operator sequence
// with NOT > AND > OR precedence. Every pass works POSITIONALLY on the item
// sequence itself — the previous implementation split items into parallel
// expressions[]/operators[] slices and paired them by index, which drifts one
// slot whenever a connector is implicit: "a=1 not b=2" negated a instead of b,
// and "a=1 b=2 or c=3" attached the OR to (a,b) instead of (b,c).
func processWithPrecedence(items []interface{}) Expr {
	if len(items) == 0 {
		return nil
	}

	// First pass: NOT (highest precedence, unary prefix). A NOT wraps the NEXT
	// expression in source order; consecutive NOTs stack ("not not a=1").
	// A trailing NOT with no operand is dropped.
	resolved := make([]interface{}, 0, len(items))
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

	// Second and third passes: the binary connectors, in precedence order.
	resolved = reduceBinary(resolved, OperationAnd)
	resolved = reduceBinary(resolved, OperationOr)

	// Any expressions still left are adjacent with no logical operator between
	// them (e.g. "a=1 b=2", or a filter following a load=/sort=/page= segment
	// that emits no connector). Combine them with an implicit AND instead of
	// silently dropping all but the first.
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

// reduceBinary combines every "expr op expr" triple for one connector,
// left-associatively, preserving source positions. A dangling connector with
// no expression on one side (leading/trailing/doubled "and"/"or") is dropped
// rather than pairing two unrelated expressions.
func reduceBinary(items []interface{}, op Operation) []interface{} {
	out := make([]interface{}, 0, len(items))
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
		if lok && rok {
			if op == OperationAnd {
				out[len(out)-1] = AndExpr{Operands: []Expr{left, right}}
			} else {
				out[len(out)-1] = OrExpr{Operands: []Expr{left, right}}
			}
			i++ // the right operand is consumed
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
	// Handle empty input
	if strings.TrimSpace(input) == "" {
		return nil
	}

	// Snapshot the plugin manager under the lock; it may be swapped
	// concurrently by SetPluginManager/RegisterPlugin.
	f.mu.RLock()
	pm := f.pluginManager
	f.mu.RUnlock()

	// Execute BeforeParse plugin hooks
	if pm != nil {
		var err error
		input, err = pm.ExecuteBeforeParse(f, input)
		if err != nil {
			return fmt.Errorf("plugin BeforeParse error: %w", err)
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

	// Execute AfterParse plugin hooks
	if pm != nil {
		err := pm.ExecuteAfterParse(f, input)
		if err != nil {
			f.mu.Lock()
			f.dsl = prevDSL
			f.mu.Unlock()
			return fmt.Errorf("plugin AfterParse error: %w", err)
		}
	}

	return nil
}

// AddFilter appends a programmatic expression. Plugin expression filters
// (e.g. FieldsPlugin's ignore/whitelist pruning) apply here exactly as they
// do to DSL input — register them BEFORE adding filters.
func (f *figo) AddFilter(exp Expr) {
	f.mu.RLock()
	pm := f.pluginManager
	f.mu.RUnlock()

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
}

func (f *figo) AddSelectFields(fields ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, field := range fields {
		f.selectFields[field] = true
	}
}

func (f *figo) GetClauses() []Expr {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Return a copy to avoid race conditions
	result := make([]Expr, len(f.clauses))
	copy(result, f.clauses)
	return result
}

func (f *figo) GetPreloads() map[string][]Expr {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Return a copy so callers can't race with (or mutate) internal state.
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

func (f *figo) SetPage(skip, take int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.page.Skip = skip
	f.page.Take = take
	f.page.validate()
}

func (f *figo) SetPageString(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	pageContent := strings.Split(v, ",")

	for _, s := range pageContent {
		pageSplit := strings.Split(s, ":")
		if len(pageSplit) != 2 {
			continue
		}

		field := pageSplit[0]
		value := pageSplit[1]

		parseInt, parsErr := strconv.ParseInt(value, 10, 64)
		if parsErr == nil {
			switch field {
			case "skip":
				f.page.Skip = int(parseInt)
			case "take":
				f.page.Take = int(parseInt)
			}

			f.page.validate()
		}

	}
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
		if q, ok := adapter.GetQuery(f, ctx, conditionType...); ok {
			if pm != nil {
				if err := pm.ExecuteAfterQuery(f, ctx, q); err != nil {
					return nil
				}
			}
			return q
		}
	}
	return nil
}

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
		f.mu.Unlock()
		// Even with no DSL, clause finalizers must run (a ScopePlugin's
		// mandatory filter applies to unfiltered queries too).
		f.finalizeClauses()
		return nil
	}

	// Clear all DSL-derived state before rebuilding so Build is idempotent:
	// preloads used to accumulate (Build();Build() duplicated every load=
	// condition) and a previous DSL's sort survived a rebuild. Page is kept —
	// it has public setters (SetPage) whose effect must outlive Build.
	f.clauses = []Expr{}
	f.preloads = make(map[string][]Expr)
	f.sort = nil

	var diags []error
	root := f.parseDSL(f.dsl, &diags)
	expressionParser(root)

	finalExpr := getFinalExpr(*root)

	// Detach the freshly parsed preloads so plugin filters can run on them
	// outside the lock (concurrent readers see an empty map until the
	// filtered results are written back below).
	preloads := f.preloads
	f.preloads = make(map[string][]Expr)
	pm := f.pluginManager
	f.mu.Unlock()

	// Run plugin expression filters (e.g. FieldsPlugin's ignore/whitelist
	// pruning) OUTSIDE the lock — a FilterExpr callback may call back into
	// this instance's read methods, which must not deadlock. Preload
	// conditions parse through the same DSL, so they are filtered as well.
	if pm != nil {
		if finalExpr != nil {
			finalExpr = pm.ExecuteExprFilters(f, finalExpr)
		}
		for table, exprs := range preloads {
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
	}

	f.mu.Lock()
	if finalExpr != nil {
		f.clauses = append(f.clauses, finalExpr)
	}
	f.preloads = preloads
	f.mu.Unlock()

	f.finalizeClauses()

	return errors.Join(diags...)
}

// finalizeClauses runs registered ClauseFinalizer plugins over the top-level
// clause list and writes the result back. Runs outside the lock (a finalizer
// may call back into read methods).
func (f *figo) finalizeClauses() {
	f.mu.RLock()
	pm := f.pluginManager
	f.mu.RUnlock()
	if pm == nil {
		return
	}

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

// GetPluginManager returns the current plugin manager
func (f *figo) GetPluginManager() *PluginManager {
	f.mu.RLock()
	defer f.mu.RUnlock()
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
