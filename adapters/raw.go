package adapters

import (
	figo "github.com/bi0dread/figo/v4"

	"database/sql/driver"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
)

// sortedKeys returns the keys of a set-like map in deterministic order, so the
// generated SQL (column lists, JOIN order) is stable across runs — important for
// query/plan caching and golden tests.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortStrings sorts a slice of names in place. It exists so adapters in this
// package can order map-derived relation names deterministically without each
// importing "sort" (gorm.go shadows that name with local sort variables).
func sortStrings(s []string) { sort.Strings(s) }

// RawPreload represents a built WHERE clause and args for a preload relationship
type RawPreload struct {
	Where string
	Args  []any
}

// BuildRawPreloads builds WHERE clauses for each preload relationship without
// any ORM dependency. An expression the raw adapter cannot render returns an
// error (and a nil map) instead of silently dropping the condition.
func BuildRawPreloads(f figo.Figo) (map[string]RawPreload, error) {
	d := rawDialectOf(f)
	result := make(map[string]RawPreload)
	for rel, exprs := range f.GetPreloads() {
		where, args, err := buildWhereFromExprs(d, exprs)
		if err != nil {
			return nil, err
		}
		if d.NumberedPlaceholders {
			where = numberPlaceholders(d, where)
		}
		result[rel] = RawPreload{Where: where, Args: args}
	}
	return result, nil
}

// BuildRawWhere builds a SQL WHERE clause (without the leading WHERE keyword)
// and its args. An expression the raw adapter cannot render returns an error
// instead of silently dropping the condition (which would widen the result).
func BuildRawWhere(f figo.Figo) (string, []any, error) {
	d := rawDialectOf(f)
	where, args, err := buildWhereFromExprs(d, clausesForRender(f))
	if err != nil {
		return "", nil, err
	}
	if d.NumberedPlaceholders {
		where = numberPlaceholders(d, where)
	}
	return where, args, nil
}

// BuildRawSelect builds a full SELECT query for the given table and columns.
// Identifier quoting and placeholder style come from the instance's raw
// adapter dialect (MySQL backticks and '?' by default). An expression the raw
// adapter cannot render returns an error instead of silently dropping it.
func BuildRawSelect(f figo.Figo, table string, columns ...string) (string, []any, error) {
	d := rawDialectOf(f)
	sql, args, err := buildFullSelect(d, f, table, columns...)
	if err != nil {
		return "", nil, err
	}
	if d.NumberedPlaceholders {
		sql = numberPlaceholders(d, sql)
	}
	return sql, args, nil
}

// -- internals --
// Internal builders always emit '?' placeholders; numbered dialects rewrite
// them ONCE on the fully assembled statement (numbering fragments and then
// concatenating them would repeat $1).

// clausesForRender returns the instance's top-level clauses for RENDERING.
//
// GetClauses is contractually a DEEP copy (hunt #6 M3: a shallow snapshot let a
// caller's write reach the live instance), which is right for an external
// inspector and wasteful on the render path, where the tree is only read and is
// walked two or three times per statement. The core exposes a read-only
// snapshot for exactly this; fall back to GetClauses for any other Figo
// implementation.
func clausesForRender(f figo.Figo) []figo.Expr {
	if r, ok := f.(interface{ ClausesForRender() []figo.Expr }); ok {
		return r.ClausesForRender()
	}
	return f.GetClauses()
}

func buildWhereFromExprs(d *SQLDialect, exprs []figo.Expr) (string, []any, error) {
	if len(exprs) == 0 {
		return "", nil, nil
	}

	// If multiple top-level expressions exist, combine with AND
	parts := make([]string, 0, len(exprs))
	args := make([]any, 0)
	for _, e := range exprs {
		if e == nil {
			continue
		}
		p, a, err := exprToSQL(d, e)
		if err != nil {
			return "", nil, err
		}
		if p != "" {
			parts = append(parts, p)
			args = append(args, a...)
		}
	}
	return strings.Join(parts, " AND "), args, nil
}

// errUnsupportedExpr builds the failure for an expression the raw adapter has
// no rendering for. Failing (instead of returning "") is deliberate: dropping
// a predicate silently widens the result set — a filter/authorization bypass.
func errUnsupportedExpr(e figo.Expr) error {
	return fmt.Errorf("raw adapter: unsupported expression type %T%s", e, docExprSupport(e))
}

// docExprSupport qualifies an unsupported-type error only when the type really
// IS rendered elsewhere. The parenthetical used to be unconditional, which made
// it a false claim for anything no adapter handles — notably a pointer-form
// node like *figo.EqExpr, which Mongo and Elasticsearch reject too.
// The list must stay in step with the advanced-expression arms of BOTH non-SQL
// adapters (mongo.go's exprToMongo and elasticsearch.go's exprToES). All five
// of figo's advanced types are rendered by both; ArrayOverlapsExpr was missing
// here, so the one type this helper exists to qualify was the one that lost the
// hint while its sibling ArrayContainsExpr kept it.
func docExprSupport(e figo.Expr) string {
	switch e.(type) {
	case figo.JsonPathExpr, figo.ArrayContainsExpr, figo.ArrayOverlapsExpr,
		figo.FullTextSearchExpr, figo.GeoDistanceExpr:
		return " (rendered by the MongoDB/Elasticsearch adapters only)"
	default:
		return ""
	}
}

func exprToSQL(d *SQLDialect, e figo.Expr) (string, []any, error) {
	// Reject a field name that quoting cannot make executable (NUL/control
	// bytes, empty dot segments) before it reaches quoteIdent — see
	// validateIdent. CustomExpr is exempt: its field is handed to the handler
	// verbatim and is never quoted by this adapter. An empty field is left to
	// the per-case rendering (an unsupported expression type also reports an
	// empty field, and must keep its own, clearer error).
	if _, isCustom := e.(figo.CustomExpr); !isCustom {
		if fld := figo.ExprField(e); fld != "" {
			if err := validateIdent("column", fld); err != nil {
				return "", nil, err
			}
		}
	}
	switch x := e.(type) {
	case figo.EqExpr:
		if x.Value == nil {
			// Canonical across adapters: a nil comparison value is the IS NULL
			// predicate ("= NULL" is never true under three-valued logic).
			return exprToSQL(d, figo.IsNullExpr{Field: x.Field})
		}
		return fmt.Sprintf("%s = ?", d.quoteIdent(x.Field)), []any{x.Value}, nil
	case figo.GteExpr:
		return fmt.Sprintf("%s >= ?", d.quoteIdent(x.Field)), []any{x.Value}, nil
	case figo.GtExpr:
		return fmt.Sprintf("%s > ?", d.quoteIdent(x.Field)), []any{x.Value}, nil
	case figo.LtExpr:
		return fmt.Sprintf("%s < ?", d.quoteIdent(x.Field)), []any{x.Value}, nil
	case figo.LteExpr:
		return fmt.Sprintf("%s <= ?", d.quoteIdent(x.Field)), []any{x.Value}, nil
	case figo.NeqExpr:
		if x.Value == nil {
			// Canonical across adapters: != nil is the IS NOT NULL predicate.
			return exprToSQL(d, figo.NotNullExpr{Field: x.Field})
		}
		return fmt.Sprintf("%s != ?", d.quoteIdent(x.Field)), []any{x.Value}, nil
	case figo.LikeExpr:
		return fmt.Sprintf("%s LIKE ?", d.quoteIdent(x.Field)), []any{x.Value}, nil
	case figo.RegexExpr:
		// The operator comes from the dialect: REGEXP (MySQL/SQLite), ~ (Postgres).
		return fmt.Sprintf("%s %s ?", d.quoteIdent(x.Field), d.RegexOperator), []any{x.Value}, nil
	case figo.ILikeExpr:
		return fmt.Sprintf("LOWER(%s) LIKE LOWER(?)", d.quoteIdent(x.Field)), []any{x.Value}, nil
	case figo.IsNullExpr:
		return fmt.Sprintf("%s IS NULL", d.quoteIdent(x.Field)), nil, nil
	case figo.NotNullExpr:
		return fmt.Sprintf("%s IS NOT NULL", d.quoteIdent(x.Field)), nil, nil
	case figo.InExpr:
		if len(x.Values) == 0 {
			// An empty IN set matches nothing. Returning "" would drop the whole
			// predicate (WHERE disappears), turning a match-nothing filter into a
			// match-everything one — a filter/authorization bypass.
			return "1=0", nil, nil
		}
		placeholders := strings.Repeat("?,", len(x.Values))
		placeholders = placeholders[:len(placeholders)-1]
		return fmt.Sprintf("%s IN (%s)", d.quoteIdent(x.Field), placeholders), append([]any{}, x.Values...), nil
	case figo.NotInExpr:
		if len(x.Values) == 0 {
			// "NOT IN (empty set)" is true for every row.
			return "1=1", nil, nil
		}
		placeholders := strings.Repeat("?,", len(x.Values))
		placeholders = placeholders[:len(placeholders)-1]
		return fmt.Sprintf("%s NOT IN (%s)", d.quoteIdent(x.Field), placeholders), append([]any{}, x.Values...), nil
	case figo.BetweenExpr:
		return fmt.Sprintf("%s BETWEEN ? AND ?", d.quoteIdent(x.Field)), []any{x.Low, x.High}, nil
	case figo.AndExpr:
		return joinGroup(d, "AND", x.Operands)
	case figo.OrExpr:
		p, a, err := joinGroup(d, "OR", x.Operands)
		if err != nil {
			return "", nil, err
		}
		if p == "" {
			// An OR with no renderable operands is a false disjunction: it must
			// match NOTHING. Returning "" dropped the predicate entirely (a
			// top-level empty OR made the WHERE clause vanish → every row
			// matched) — the same fail-open the empty-IN guard above closes,
			// and that Mongo ({"$nor":[{}]}) and Elasticsearch
			// ({"match_none":{}}) also render explicitly. ES did NOT prevent it
			// before hunt #7: an empty "should" list is zero Lucene clauses, and
			// a clause-less bool query is match_all before minimum_should_match
			// is even applied.
			// An empty AND stays dropped: it is the true identity.
			return "1=0", nil, nil
		}
		return p, a, nil
	case figo.NotExpr:
		if !hasNonNilOperand(x.Operands) {
			return "", nil, nil
		}
		// figo.NotExpr means "none of the operands match": NOT(a OR b), matching
		// Mongo's $nor and GORM's clause.Not. Joining with AND here rendered
		// NOT(a AND b), which matches rows the other adapters exclude.
		inner, args, err := joinGroup(d, "OR", x.Operands)
		if err != nil {
			return "", nil, err
		}
		if inner == "" {
			// The operands rendered no predicate (vacuously true, e.g. an
			// empty AND identity), so the negation matches NOTHING. Returning
			// "" dropped the NOT entirely: fail open.
			return "1=0", nil, nil
		}
		return fmt.Sprintf("NOT (%s)", inner), args, nil
	case figo.CustomExpr:
		// The handler owns the fragment: it receives the field verbatim
		// (unquoted) and returns SQL with '?' placeholders plus bind args.
		if x.Handler == nil {
			return "", nil, fmt.Errorf("raw adapter: CustomExpr on field %q has no handler", x.Field)
		}
		frag, args, err := x.Handler(x.Field, x.Operator, x.Value)
		if err != nil {
			return "", nil, fmt.Errorf("raw adapter: CustomExpr handler for field %q: %w", x.Field, err)
		}
		if frag == "" {
			// An empty fragment contributes nothing (its args were already
			// discarded by the callers' p != "" test).
			return "", nil, nil
		}
		frag, args = expandSliceArgs(d, frag, args)
		// Parenthesize the fragment. It is spliced into an " AND " join, and
		// SQL's AND binds tighter than OR, so a compound fragment silently
		// re-associated: scope AND (a OR b) rendered as a OR (b AND scope),
		// returning rows outside a mandatory ScopePlugin filter. Every other
		// case here is self-delimiting; the GORM adapter, documented as sharing
		// this handler contract, already nests the fragment correctly.
		return "(" + frag + ")", args, nil
	case figo.OrderBy:
		// Rendered by buildOrderBy, which reads the clause list as well as
		// GetSort — not dropped (see buildOrderBy).
		return "", nil, nil
	default:
		return "", nil, errUnsupportedExpr(e)
	}
}

// hasNonNilOperand reports whether the operand list has at least one real
// entry — NOT() with no operands is the vacuous-true identity, but NOT over
// operands that merely RENDER empty must fail closed instead.
func hasNonNilOperand(ops []figo.Expr) bool {
	for _, op := range ops {
		if op != nil {
			return true
		}
	}
	return false
}

// joinGroup renders a boolean group. The whole subtree goes into ONE buffer
// (see sqlWriter) — returning a fresh string per nesting level re-copied the
// accumulated fragment at every level, so a tree the round-4 flattening cannot
// collapse (alternating and/or, or a not-chain) cost O(output x depth): a
// 152 KB filter burned 1.5 s and 5.7 GB of allocation to produce 216 KB of SQL,
// while the other three adapters stayed linear on the same AST.
func joinGroup(d *SQLDialect, op string, operands []figo.Expr) (string, []any, error) {
	var w sqlWriter
	wrote, err := w.writeGroup(d, op, operands)
	if err != nil {
		return "", nil, err
	}
	if !wrote {
		return "", nil, nil
	}
	return string(w.buf), w.args, nil
}

// sqlWriter accumulates a rendered expression tree in place. Every method
// reports whether it wrote a predicate, so an operand that renders to nothing
// is rolled back (buffer AND args) exactly as the string-based joiner dropped
// an empty part.
type sqlWriter struct {
	buf  []byte
	args []any
}

// writeGroup joins the operands with op, parenthesizing only when more than one
// of them renders — byte-for-byte the output the string joiner produced.
func (w *sqlWriter) writeGroup(d *SQLDialect, op string, operands []figo.Expr) (bool, error) {
	nonNil := 0
	for _, e := range operands {
		if e != nil {
			nonNil++
		}
	}
	if nonNil == 0 {
		return false, nil
	}
	start := len(w.buf)
	// Decide the paren before rendering (an insert afterwards would have to
	// shift the group's bytes, reintroducing the quadratic). The rare case
	// where operands drop and only one part survives is fixed up below.
	paren := nonNil > 1
	if paren {
		w.buf = append(w.buf, '(')
	}
	n := 0
	for _, e := range operands {
		if e == nil {
			continue
		}
		mark, argMark := len(w.buf), len(w.args)
		if n > 0 {
			w.buf = append(w.buf, ' ')
			w.buf = append(w.buf, op...)
			w.buf = append(w.buf, ' ')
		}
		wrote, err := w.writeExpr(d, e)
		if err != nil {
			return false, err
		}
		if !wrote {
			w.buf = w.buf[:mark]
			w.args = w.args[:argMark]
			continue
		}
		n++
	}
	if n == 0 {
		w.buf = w.buf[:start]
		return false, nil
	}
	if paren {
		if n == 1 {
			// Only one operand rendered after all: drop the speculative '('.
			copy(w.buf[start:], w.buf[start+1:])
			w.buf = w.buf[:len(w.buf)-1]
		} else {
			w.buf = append(w.buf, ')')
		}
	}
	return true, nil
}

// writeExpr renders one expression. The logical nodes recurse in place; every
// other type is a leaf whose rendering is short, so it goes through exprToSQL.
func (w *sqlWriter) writeExpr(d *SQLDialect, e figo.Expr) (bool, error) {
	switch x := e.(type) {
	case figo.AndExpr:
		return w.writeGroup(d, "AND", x.Operands)
	case figo.OrExpr:
		wrote, err := w.writeGroup(d, "OR", x.Operands)
		if err != nil {
			return false, err
		}
		if !wrote {
			// See exprToSQL: an empty OR must match NOTHING, not vanish.
			w.buf = append(w.buf, "1=0"...)
		}
		return true, nil
	case figo.NotExpr:
		if !hasNonNilOperand(x.Operands) {
			return false, nil
		}
		mark, argMark := len(w.buf), len(w.args)
		// See exprToSQL: figo.NotExpr is NOT(a OR b) across all adapters.
		w.buf = append(w.buf, "NOT ("...)
		wrote, err := w.writeGroup(d, "OR", x.Operands)
		if err != nil {
			return false, err
		}
		if !wrote {
			// See exprToSQL: NOT over a vacuously-true operand matches NOTHING.
			w.buf = w.buf[:mark]
			w.args = w.args[:argMark]
			w.buf = append(w.buf, "1=0"...)
			return true, nil
		}
		w.buf = append(w.buf, ')')
		return true, nil
	default:
		p, a, err := exprToSQL(d, e)
		if err != nil {
			return false, err
		}
		if p == "" {
			return false, nil
		}
		w.buf = append(w.buf, p...)
		w.args = append(w.args, a...)
		return true, nil
	}
}

// buildOrderBy renders ORDER BY from the instance's sort AND from any
// figo.OrderBy node in the clause list. The clause-list form used to render
// nowhere on this adapter (exprToSQL returns "" for it and this function only
// read GetSort), while the GORM adapter applied it — the same figo state came
// back in a different order, and with a LIMIT in force that means different
// ROWS. Clause-list columns come first, then GetSort's, matching the order
// ApplyGorm applies them in.
func buildOrderBy(d *SQLDialect, f figo.Figo, clauses []figo.Expr) (string, error) {
	cols := make([]string, 0, 4)
	add := func(ob figo.OrderBy) error {
		for _, c := range ob.Columns {
			// Defensively skip empty column names: they would render ORDER BY ``.
			if c.Name == "" {
				continue
			}
			if err := validateIdent("sort key", c.Name); err != nil {
				return err
			}
			dir := "ASC"
			if c.Desc {
				dir = "DESC"
			}
			cols = append(cols, fmt.Sprintf("%s %s", d.quoteIdent(c.Name), dir))
		}
		return nil
	}
	for _, e := range clauses {
		if ob, ok := e.(figo.OrderBy); ok {
			if err := add(ob); err != nil {
				return "", err
			}
		}
	}
	if s := f.GetSort(); s != nil {
		if err := add(*s); err != nil {
			return "", err
		}
	}
	if len(cols) == 0 {
		return "", nil
	}
	return "ORDER BY " + strings.Join(cols, ", "), nil
}

func buildLimitOffset(d *SQLDialect, f figo.Figo) string {
	p := f.GetPage()
	// Embed numbers directly for broad driver compatibility
	// If Take is 0, skip LIMIT clause
	if p.Take <= 0 && p.Skip <= 0 {
		return ""
	}
	if p.Take > 0 && p.Skip > 0 {
		return fmt.Sprintf("LIMIT %d OFFSET %d", p.Take, p.Skip)
	}
	if p.Take > 0 {
		return fmt.Sprintf("LIMIT %d", p.Take)
	}
	// OFFSET without LIMIT is a syntax error on MySQL/SQLite, so pair it with
	// the dialect's "unbounded" LIMIT token (MySQL max-uint64, Postgres ALL,
	// SQLite -1).
	return fmt.Sprintf("LIMIT %s OFFSET %d", d.NoLimitToken, p.Skip)
}

// AdapterRawGetSql is an internal helper to integrate with figo.GetSqlString
// ctx can be a table name string or a struct containing Table.
type RawContext struct {
	Table string
}

// AdapterRawGetSql renders the statement with '?' placeholders regardless of
// dialect; callers that hand args to a driver convert via the adapter
// (RawAdapter.GetQuery numbers them for Postgres). It errors on a ctx that is
// neither a table name string nor a RawContext, and on any expression the raw
// adapter cannot render (nothing is silently dropped).
func AdapterRawGetSql(f figo.Figo, ctx any, conditionType ...string) (string, []any, error) {
	return rawGetSQL(rawDialectOf(f), f, ctx, conditionType...)
}

// rawGetSQL is AdapterRawGetSql with the dialect supplied by the caller. The
// RawAdapter methods pass their own receiver's dialect: an instance built with
// nil, with another adapter, or with a *RawAdapter holds no value RawAdapter
// for rawDialectOf to read, and taking quoting from the instance while taking
// placeholder numbering from the receiver produced MySQL backticks with $n
// binds — valid on no engine.
func rawGetSQL(d *SQLDialect, f figo.Figo, ctx any, conditionType ...string) (string, []any, error) {
	switch v := ctx.(type) {
	case string:
		return buildByConditions(d, f, v, conditionType...)
	case RawContext:
		return buildByConditions(d, f, v.Table, conditionType...)
	default:
		return "", nil, fmt.Errorf("raw adapter: unsupported ctx type %T (want a table name string or RawContext)", ctx)
	}
}

// RawAdapter renders standalone SQL. The zero value uses the MySQL dialect
// (backtick identifiers, ? placeholders, REGEXP); set Dialect to
// PostgresDialect / SQLiteDialect (or a custom *SQLDialect) to change the
// rendering. Select the dialect BEFORE rendering, e.g. Build(RawAdapter{
// Dialect: figo.PostgresDialect}).
type RawAdapter struct {
	Dialect *SQLDialect
}

// dialect returns the configured dialect, defaulting to MySQL.
func (a RawAdapter) dialect() *SQLDialect {
	if a.Dialect == nil {
		return MySQLDialect
	}
	return a.Dialect
}

// GetSqlString renders the requested segments with literals interpolated.
//
// ok=false MEANS THE RENDER FAILED AND THE STRING IS NOT A STATEMENT. The
// returned string is always "" in that case, and a caller composing segments
// ("SELECT * FROM items " + <the WHERE segment>) must abort — splicing "" turns
// a failed render into an unfiltered statement. Note that figo.GetSqlString
// (the facade) has no error channel and returns exactly that "": treat an empty
// segment result as an error, never as "there was no predicate". Use
// RawAdapter.GetSqlString/GetQuery directly, or BuildE, when you need to tell
// the two apart.
func (a RawAdapter) GetSqlString(f figo.Figo, ctx any, conditionType ...string) (string, bool) {
	if f == nil {
		return "", false
	}
	// A build error fails the render (ok=false) — fail closed rather than
	// returning SQL that silently omits a predicate.
	sql, args, err := rawGetSQL(a.dialect(), f, ctx, conditionType...)
	if err != nil {
		return "", false
	}
	// Interpolate literals into the ?-form; numbering never applies here.
	return expandPlaceholders(a.dialect(), sql, args), true
}

// GetQuery renders the requested segments in bind form. As with GetSqlString,
// ok=false means the render failed and the Query is nil — never a statement with
// a missing predicate.
func (a RawAdapter) GetQuery(f figo.Figo, ctx any, conditionType ...string) (figo.Query, bool) {
	if f == nil {
		return nil, false
	}
	sql, args, err := rawGetSQL(a.dialect(), f, ctx, conditionType...)
	if err != nil {
		return nil, false
	}
	if a.dialect().NumberedPlaceholders {
		sql = numberPlaceholders(a.dialect(), sql)
	}
	return figo.SQLQuery{SQL: sql, Args: args}, true
}

func buildByConditions(d *SQLDialect, f figo.Figo, table string, conditionType ...string) (string, []any, error) {
	// No conditionType: the whole SELECT. Answered BEFORE the per-segment
	// builders below, which buildFullSelect renders itself — computing them
	// here first and then discarding them did every join/where/order render
	// twice on the adapter's main path (measurably 2x on large filters, on top
	// of whatever those renders cost).
	if len(conditionType) == 0 {
		return buildFullSelect(d, f, table)
	}

	// Render (and validate) ONLY the inputs the caller's segment list actually
	// consumes. Doing all of them up front let an input that a single segment
	// owns fail a render that never asked for it: an absent table name (read
	// only by FROM), an unrenderable select field (only SELECT) or an
	// unrenderable sort key (only ORDER BY/SORT) each turned a "WHERE"-only
	// render into an error. Each of those segments still fails closed on its
	// OWN inputs — that is the A10/A7-5 + A7-6 + M11 policy and it is intact —
	// but the segment API exists so a caller can splice one fragment into its
	// own statement, and figo.GetSqlString has no error channel: it collapses
	// ok=false to "", so a poisoned WHERE segment became an unfiltered
	// statement at the caller. Scoping the work to the requested segments is
	// also strictly less work than before.
	var needCols, needTable, needWhere, needOrder, needLimit bool
	for _, ct := range conditionType {
		switch normalizeConditionType(ct) {
		case "SELECT":
			needCols = true
		case "FROM":
			needTable = true
		case "WHERE", "LIKE":
			needWhere = true
		case "ORDER BY", "SORT":
			needOrder = true
		case "LIMIT", "OFFSET", "PAGE":
			needLimit = true
		}
		// Unrecognized keywords are deliberately NOT rejected here: the segment
		// loop below owns that error (L2), so its message and its precedence
		// relative to a bad identifier stay exactly as they were.
	}

	cols := "*"
	if needCols {
		var err error
		if cols, err = columnsOnly(f, d); err != nil {
			return "", nil, err
		}
	}
	if needTable {
		if err := validateIdent("table", table); err != nil {
			return "", nil, err
		}
	}
	// One snapshot for both segments: the clause tree is read-only here, and
	// GetClauses' deep copy is expensive on a large tree (hunt #8 A10-2/R5).
	var clauses []figo.Expr
	if needWhere || needOrder {
		clauses = clausesForRender(f)
	}
	var (
		where     string
		whereArgs []any
	)
	if needWhere {
		var err error
		if where, whereArgs, err = buildWhereFromExprs(d, clauses); err != nil {
			return "", nil, err
		}
	}
	var orderBy string
	if needOrder {
		var err error
		if orderBy, err = buildOrderBy(d, f, clauses); err != nil {
			return "", nil, err
		}
	}
	var limitOffset string
	if needLimit {
		limitOffset = buildLimitOffset(d, f)
	}

	// Build only requested parts, in the order provided
	parts := make([]string, 0, len(conditionType)*2)
	args := make([]any, 0)
	whereAdded := false
	orderAdded := false
	limitAdded := false
	offsetAdded := false
	for _, ct := range conditionType {
		norm := normalizeConditionType(ct)
		switch norm {
		case "SELECT":
			parts = append(parts, fmt.Sprintf("SELECT %s", cols))
		case "FROM":
			parts = append(parts, fmt.Sprintf("FROM %s", d.quoteIdent(table)))
		case "WHERE", "LIKE":
			// "WHERE" and "LIKE" are aliases for the same clause; guard against
			// emitting it (and its args) twice when both are requested, which
			// would also misalign every later placeholder.
			if where != "" && !whereAdded {
				parts = append(parts, "WHERE "+where)
				args = append(args, whereArgs...)
				whereAdded = true
			}
		case "ORDER BY", "SORT":
			// "ORDER BY" and "SORT" are aliases; emit the clause at most once.
			if orderBy != "" && !orderAdded {
				parts = append(parts, orderBy)
				orderAdded = true
			}
		case "LIMIT":
			// Emit only the LIMIT part: "LIMIT" alone must not leak the
			// OFFSET, and a repeated (or PAGE-covered) LIMIT must not emit
			// "LIMIT 10 LIMIT 10", which no engine accepts.
			if strings.HasPrefix(limitOffset, "LIMIT ") && !limitAdded {
				if idx := strings.Index(limitOffset, " OFFSET "); idx >= 0 {
					parts = append(parts, limitOffset[:idx])
				} else {
					parts = append(parts, limitOffset)
				}
				limitAdded = true
			}
		case "OFFSET":
			if idx := strings.Index(limitOffset, "OFFSET "); idx >= 0 && !offsetAdded {
				parts = append(parts, limitOffset[idx:])
				offsetAdded = true
			}
		case "PAGE":
			// PAGE is the LIMIT+OFFSET pair; skip it once either half is out.
			if limitOffset != "" && !limitAdded && !offsetAdded {
				parts = append(parts, limitOffset)
				limitAdded = true
				offsetAdded = true
			}
		case "JOIN":
			// Recognized, emits nothing: preloads no longer render as JOINs
			// (see buildFullSelect). Kept as a no-op rather than an error so
			// existing callers listing JOIN among their segments keep working.
		case "GROUP BY":
			// Not supported in core; tolerate silently
		default:
			// An unrecognized keyword used to be dropped in silence, so a typo
			// ("ORDERBY", "WHRE") returned a statement missing that clause with
			// ok=true — the same silent-omission failure the adapter refuses to
			// make everywhere else.
			return "", nil, fmt.Errorf("raw adapter: unrecognized conditionType %q (want SELECT, FROM, JOIN, WHERE, ORDER BY/SORT, LIMIT, OFFSET, PAGE or GROUP BY)", ct)
		}
	}

	fullSQL := strings.Join(parts, " ")
	return fullSQL, args, nil
}

// buildFullSelect assembles the complete SELECT in ?-form (numbering, when the
// dialect requires it, happens at the adapter/helper boundary). Explicit
// columns are used only when the instance has no select fields.
// A preload is NOT rendered into this statement. It used to become
// "JOIN <table> ON <preload filter>" — a join with no key condition, so it
// multiplied the primary rows (2 users x 2 orders = 4 rows) or annihilated them
// (a preload filter matching nothing emptied the main result), and it made the
// unqualified main WHERE ambiguous for any column both tables have ("ambiguous
// column name: id"). No other adapter lets "preload this relation" change the
// primary row set: GORM issues a separate Preload query, Mongo adds a field, ES
// fails the build. The raw adapter has no schema metadata, so it cannot express
// the join key — it now leaves the main statement alone, and the preload
// filters stay available as rendered fragments via BuildRawPreloads for callers
// running their own join/second query.
func buildFullSelect(d *SQLDialect, f figo.Figo, table string, columns ...string) (string, []any, error) {
	cols, err := columnsOnly(f, d)
	if err != nil {
		return "", nil, err
	}
	if cols == "*" && len(columns) > 0 {
		quoted := make([]string, 0, len(columns))
		for _, c := range columns {
			if err := validateIdent("column", c); err != nil {
				return "", nil, err
			}
			quoted = append(quoted, d.quoteIdent(c))
		}
		cols = strings.Join(quoted, ", ")
	}
	if err := validateIdent("table", table); err != nil {
		return "", nil, err
	}

	clauses := clausesForRender(f)
	where, whereArgs, err := buildWhereFromExprs(d, clauses)
	if err != nil {
		return "", nil, err
	}
	orderBy, err := buildOrderBy(d, f, clauses)
	if err != nil {
		return "", nil, err
	}
	limitOffset := buildLimitOffset(d, f)

	query := fmt.Sprintf("SELECT %s FROM %s", cols, d.quoteIdent(table))
	if where != "" {
		query += " WHERE " + where
	}
	if orderBy != "" {
		query += " " + orderBy
	}
	if limitOffset != "" {
		query += " " + limitOffset
	}
	args := append([]any{}, whereArgs...)
	return query, args, nil
}

// columnsOnly renders the SELECT column list from the instance's selects.
func columnsOnly(f figo.Figo, d *SQLDialect) (string, error) {
	sel := f.GetSelectFields()
	if len(sel) == 0 {
		return "*", nil
	}
	quoted := make([]string, 0, len(sel))
	seen := make(map[string]bool, len(sel))
	for _, name := range sortedKeys(sel) {
		col := normalizeColumnName(f, name)
		// Dedupe AFTER normalization: distinct spellings of one column
		// ("userName", "user_name", "UserName") collapse to the same name and
		// were emitted once each — SELECT "user_name", "user_name", "user_name".
		if seen[col] {
			continue
		}
		seen[col] = true
		// An empty (or all-dot) select field rendered SELECT "" — rejected by
		// Postgres and MySQL, a constant column on SQLite. buildOrderBy has had
		// this guard; the projection did not.
		if err := validateIdent("select field", col); err != nil {
			return "", err
		}
		quoted = append(quoted, d.quoteIdent(col))
	}
	return strings.Join(quoted, ", "), nil
}

func normalizeConditionType(s string) string {
	up := strings.ToUpper(strings.TrimSpace(s))
	switch up {
	case "SORT":
		return "SORT"
	case "PAGE":
		return "PAGE"
	case "ORDERBY", "ORDER":
		// Spelled without the space the way GROUPBY/GROUP are accepted below;
		// "ORDERBY" used to fall through and drop the ORDER BY clause.
		return "ORDER BY"
	case "GROUPBY", "GROUP", "GORUP BY", "GORUPBY":
		return "GROUP BY"
	default:
		return up
	}
}

// expandSliceArgs rewrites a CustomExpr handler fragment so a slice bound to a
// '?' that immediately follows '(' becomes one placeholder per element, and
// flattens that arg. This is GORM's clause.Expr rule, and the GORM adapter
// states it shares this adapter's handler contract — so the natural
// "id IN (?)" handler worked there and died here at the driver with
// "unsupported type []interface {}, a slice of interface", since database/sql
// cannot bind a slice.
//
// Values a driver CAN bind are left alone: []byte (and any other byte slice),
// and anything implementing driver.Valuer such as pq.Int64Array. When no arg is
// expandable — the overwhelmingly common case — the fragment is returned
// untouched, so a working handler is never rewritten.
//
// It honours the same backslash-escape rule as expandPlaceholders and
// numberPlaceholders (hunt #6 L4): on a dialect where '\' escapes inside a
// single-quoted literal (MySQL), a fragment containing \' otherwise flipped
// THIS scanner out of the literal while the other two stayed in it, so the two
// disagreed about whether a later '?' was a placeholder — the slice was left
// unexpanded and handed to database/sql (which cannot bind it) while the debug
// SQL showed a fully expanded IN list.
func expandSliceArgs(d *SQLDialect, frag string, args []any) (string, []any) {
	expandable := false
	for _, a := range args {
		if _, ok := sliceArgLen(a); ok {
			expandable = true
			break
		}
	}
	if !expandable {
		return frag, args
	}

	var b strings.Builder
	b.Grow(len(frag) + 8*len(args))
	out := make([]any, 0, len(args))
	idx := 0
	prev := byte(0) // last non-space byte written, to spot the "(?" shape
	inSingle, inDouble, inBacktick := false, false, false
	for i := 0; i < len(frag); i++ {
		ch := frag[i]
		switch {
		case ch == '\\' && inSingle && d.EscapeBackslash && i+1 < len(frag):
			// A backslash inside a single-quoted literal consumes the next byte
			// on this dialect, so it must not be allowed to close the literal.
			// Same clause as expandPlaceholders/numberPlaceholders: all three
			// scanners have to agree on where the literals are.
			b.WriteByte(ch)
			i++
			ch = frag[i]
			b.WriteByte(ch)
			if ch != ' ' && ch != '\t' && ch != '\n' && ch != '\r' {
				prev = ch
			}
			continue
		case ch == '\'' && !inDouble && !inBacktick:
			inSingle = !inSingle
		case ch == '"' && !inSingle && !inBacktick:
			inDouble = !inDouble
		case ch == '`' && !inSingle && !inDouble:
			inBacktick = !inBacktick
		case ch == '?' && !inSingle && !inDouble && !inBacktick && idx < len(args):
			arg := args[idx]
			idx++
			n, ok := sliceArgLen(arg)
			if !ok || prev != '(' {
				b.WriteByte(ch)
				out = append(out, arg)
				prev = ch
				continue
			}
			if n == 0 {
				// Matches GORM: an empty list renders NULL, so "IN (NULL)"
				// matches nothing instead of being a syntax error.
				b.WriteString("NULL")
				prev = 'L'
				continue
			}
			rv := reflect.ValueOf(arg)
			for j := 0; j < n; j++ {
				if j > 0 {
					b.WriteByte(',')
				}
				b.WriteByte('?')
				out = append(out, rv.Index(j).Interface())
			}
			prev = '?'
			continue
		}
		b.WriteByte(ch)
		if ch != ' ' && ch != '\t' && ch != '\n' && ch != '\r' {
			prev = ch
		}
	}
	// Any arg the fragment had no placeholder for is preserved in order, so a
	// mismatched handler fails exactly the way it did before.
	for ; idx < len(args); idx++ {
		out = append(out, args[idx])
	}
	return b.String(), out
}

// sliceArgLen reports the element count of a bind arg that should be expanded
// into a placeholder list, and whether it is one at all.
func sliceArgLen(v any) (int, bool) {
	if v == nil {
		return 0, false
	}
	if _, ok := v.(driver.Valuer); ok {
		return 0, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return 0, false // []byte and friends bind directly
		}
		return rv.Len(), true
	default:
		return 0, false
	}
}

// expandPlaceholders replaces '?' with SQL literals derived from args in order.
// This is intended for debugging/logging, similar to GORM's DryRun Explain.
func expandPlaceholders(d *SQLDialect, sql string, args []any) string {
	if len(args) == 0 {
		return sql
	}
	var b strings.Builder
	b.Grow(len(sql) + len(args)*4)

	idx := 0
	inSingle := false
	inDouble := false
	inBacktick := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		// On dialects where '\' escapes inside a string literal, it consumes
		// the next byte: a CustomExpr fragment containing \' otherwise flipped
		// the scanner out of the literal, and every following '?' was expanded
		// against the wrong arg (or left unexpanded).
		if ch == '\\' && inSingle && d.EscapeBackslash && i+1 < len(sql) {
			b.WriteByte(ch)
			i++
			b.WriteByte(sql[i])
			continue
		}
		if ch == '\'' && !inDouble && !inBacktick {
			inSingle = !inSingle
			b.WriteByte(ch)
			continue
		}
		if ch == '"' && !inSingle && !inBacktick {
			inDouble = !inDouble
			b.WriteByte(ch)
			continue
		}
		// Track backtick-quoted identifiers too: a '?' inside `a?b` is part of
		// the column name, not a bind placeholder. Without this the value was
		// spliced into the identifier and every later placeholder bound to the
		// wrong arg.
		if ch == '`' && !inSingle && !inDouble {
			inBacktick = !inBacktick
			b.WriteByte(ch)
			continue
		}
		if ch == '?' && !inSingle && !inDouble && !inBacktick && idx < len(args) {
			b.WriteString(toSQLLiteral(d, args[idx]))
			idx++
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func toSQLLiteral(d *SQLDialect, v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int, int8, int16, int32, int64:
		return fmt.Sprintf("%v", x)
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%v", x)
	case float32:
		return floatLiteral(float64(x))
	case float64:
		return floatLiteral(x)
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case time.Time:
		// Render as a SQL datetime literal, not Go's "... +0000 UTC" String().
		// Keep the fractional seconds and the zone offset: dropping them made
		// the displayed statement denote a DIFFERENT instant than the bound
		// parameter, on the path documented as the debugging view of the query.
		return "'" + x.Format("2006-01-02 15:04:05.999999999-07:00") + "'"
	case string:
		return "'" + d.escapeString(x) + "'"
	default:
		// Fallback: quote stringified value
		return "'" + d.escapeString(fmt.Sprintf("%v", x)) + "'"
	}
}

// floatLiteral renders a float literal, guarding non-finite values: NaN/Inf
// would be spliced verbatim as invalid SQL. This display path (see
// expandPlaceholders) has no error channel, so render NULL instead — a NULL
// comparison is never true, failing closed.
func floatLiteral(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "NULL"
	}
	return fmt.Sprintf("%v", v)
}

func normalizeColumnName(f figo.Figo, name string) string {
	// Apply the instance's naming func — the same conversion the parser runs,
	// so a deliberately preserved camelCase name is not re-snake_cased here.
	naming := f.GetNamingFunc() // never nil: SnakeCaseNaming is the default
	// Convert PER '.'-SEPARATED SEGMENT, which is what the core does to a
	// filter field (figo.go's normalizeFieldName) and what quoteIdent assumes.
	// Converting the whole string folded the qualifier into the column, because
	// SnakeCaseNaming treats '.' as a word separator: a projection of
	// "users.firstName" became `users_first_name` while the SAME name in the
	// WHERE clause rendered `users`.`first_name` — one identifier spelled two
	// ways in one statement, and the projection's spelling names no column.
	if !strings.Contains(name, ".") {
		return naming(name)
	}
	segs := strings.Split(name, ".")
	for i, s := range segs {
		if s == "" {
			// An empty segment is left alone so it stays empty and reaches
			// validateIdent, which rejects it (A10/A7-6). Handing "" to a naming
			// func could turn it into a non-empty name and slip past that guard.
			continue
		}
		segs[i] = naming(s)
	}
	return strings.Join(segs, ".")
}
