package adapters

import (
	figo "github.com/bi0dread/figo/v4"

	"context"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"unicode"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// toGormClauseWithFigo converts an internal figo.Expr tree into a gorm
// clause.Expression. An expression this adapter has no rendering for returns
// an error — dropping it silently would widen the result set (a
// filter/authorization bypass).
//
// Field names are used VERBATIM: the parser already ran them through the
// instance's NamingFunc, exactly like the raw/Mongo/Elasticsearch adapters
// assume. Re-applying the func here was invisible with the idempotent
// snake_case default but rendered nonexistent columns (t_t_age) for any
// non-idempotent naming strategy.
func toGormClauseWithFigo(e figo.Expr, f figo.Figo) (clause.Expression, error) {
	getFieldName := func(field string) string { return field }

	// convertOperands maps a logical node's operand list, propagating the
	// first conversion error.
	convertOperands := func(operands []figo.Expr) ([]clause.Expression, error) {
		var parts []clause.Expression
		for _, op := range operands {
			if op == nil {
				continue
			}
			part, err := toGormClauseWithFigo(op, f)
			if err != nil {
				return nil, err
			}
			if part == nil {
				continue
			}
			parts = append(parts, part)
		}
		return parts, nil
	}

	switch x := e.(type) {
	case figo.EqExpr:
		return clause.Eq{Column: getFieldName(x.Field), Value: x.Value}, nil
	case figo.GteExpr:
		return clause.Gte{Column: getFieldName(x.Field), Value: x.Value}, nil
	case figo.GtExpr:
		return clause.Gt{Column: getFieldName(x.Field), Value: x.Value}, nil
	case figo.LtExpr:
		return clause.Lt{Column: getFieldName(x.Field), Value: x.Value}, nil
	case figo.LteExpr:
		return clause.Lte{Column: getFieldName(x.Field), Value: x.Value}, nil
	case figo.NeqExpr:
		return clause.Neq{Column: getFieldName(x.Field), Value: x.Value}, nil
	case figo.LikeExpr:
		return clause.Like{Column: getFieldName(x.Field), Value: x.Value}, nil
	case figo.RegexExpr:
		// Use configurable regex operator; default REGEXP, set to ~ or ~* for Postgres.
		return clause.Expr{SQL: fmt.Sprintf("? %s ?", figo.GetRegexSQLOperator()), Vars: []any{clause.Column{Name: getFieldName(x.Field)}, x.Value}}, nil
	case figo.ILikeExpr:
		// GORM has no ILIKE portable operator; fallback to LOWER(col) LIKE LOWER(?)
		return clause.Expr{SQL: "LOWER(?) LIKE LOWER(?)", Vars: []any{clause.Column{Name: getFieldName(x.Field)}, x.Value}}, nil
	case figo.IsNullExpr:
		return clause.Eq{Column: getFieldName(x.Field), Value: nil}, nil
	case figo.NotNullExpr:
		return clause.Neq{Column: getFieldName(x.Field), Value: nil}, nil
	case figo.InExpr:
		if len(x.Values) == 0 {
			// Empty IN matches nothing. Mirror the raw adapter's 1=0 rather than
			// relying on GORM's clause.IN, which for zero values emits "IN (NULL)".
			return clause.Expr{SQL: "1=0"}, nil
		}
		return clause.IN{Column: getFieldName(x.Field), Values: x.Values}, nil
	case figo.NotInExpr:
		if len(x.Values) == 0 {
			// "NOT IN (empty set)" is true for every row. GORM's
			// clause.Not(clause.IN{}) instead emits "col IS NOT NULL", which
			// wrongly excludes NULL rows and diverges from the raw adapter's 1=1.
			return clause.Expr{SQL: "1=1"}, nil
		}
		return clause.Not(clause.IN{Column: getFieldName(x.Field), Values: x.Values}), nil
	case figo.BetweenExpr:
		return clause.Expr{SQL: "? BETWEEN ? AND ?", Vars: []any{clause.Column{Name: getFieldName(x.Field)}, x.Low, x.High}}, nil
	case figo.AndExpr:
		parts, err := convertOperands(x.Operands)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			// Empty AND is the true identity: no predicate, consistent with
			// the raw adapter and Mongo's empty-$and ({}) rendering.
			return nil, nil
		}
		return clause.And(parts...), nil
	case figo.OrExpr:
		parts, err := convertOperands(x.Operands)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			// An OR with no renderable operands is a false disjunction: it
			// must match NOTHING. clause.Or() with zero parts emitted no
			// predicate at all — matching everything, the same fail-open the
			// empty-IN guard above closes and that Mongo ({"$nor":[{}]}) and
			// Elasticsearch ({"match_none":{}}) also render explicitly.
			return clause.Expr{SQL: "1=0"}, nil
		}
		if len(parts) == 1 {
			// An OR of ONE operand is that operand (the raw adapter's
			// joinGroup returns parts[0] here too). clause.Or of a single
			// expression yields clause.OrConditions{1 expr}, a shape GORM's
			// clause.Where treats as an "or condition": buildExprs joins it to
			// the PRECEDING clause with " OR " instead of " AND ", and
			// Where.Build's position swap moves it off index 0 to make that
			// happen. A single-operand OrExpr therefore turned every sibling
			// top-level clause into a disjunct — including a ScopePlugin's
			// mandatory tenant filter, which became optional.
			return parts[0], nil
		}
		return clause.Or(parts...), nil
	case figo.NotExpr:
		parts, err := convertOperands(x.Operands)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			hasOperand := false
			for _, op := range x.Operands {
				if op != nil {
					hasOperand = true
					break
				}
			}
			if !hasOperand {
				// NOT of nothing is vacuously true: no predicate, like Mongo's
				// empty $nor.
				return nil, nil
			}
			// The operands rendered no predicate (vacuously true, e.g. an
			// empty AND identity), so the negation matches NOTHING. Returning
			// nil dropped the NOT entirely: fail open.
			return clause.Expr{SQL: "1=0"}, nil
		}
		// NotExpr is NOR: NOT(op1 OR op2 ...). clause.Not is unusable here —
		// it unwraps a lone AndConditions and distributes the negation over
		// its conjuncts, turning NOT(a AND b) (NAND) into NOT a AND NOT b
		// (NOR). Rendering "NOT (<inner>)" verbatim preserves the semantics
		// for every operand shape.
		var inner clause.Expression
		if len(parts) == 1 {
			inner = parts[0]
		} else {
			inner = clause.Or(parts...)
		}
		return clause.Expr{SQL: "NOT (?)", Vars: []any{inner}}, nil
	case figo.CustomExpr:
		// Same contract as the raw adapter: the handler receives the field
		// verbatim and returns a SQL fragment with '?' placeholders + args.
		if x.Handler == nil {
			return nil, fmt.Errorf("gorm adapter: CustomExpr on field %q has no handler", x.Field)
		}
		frag, args, err := x.Handler(x.Field, x.Operator, x.Value)
		if err != nil {
			return nil, fmt.Errorf("gorm adapter: CustomExpr handler for field %q: %w", x.Field, err)
		}
		return clause.Expr{SQL: frag, Vars: gormProtectByteSlices(args)}, nil
	case figo.OrderBy:
		// A sort spec is not a boolean predicate, so it renders as nothing in
		// an expression position — exactly what the raw adapter does with one.
		// Returning a clause.OrderBy here was only safe where trx.Clauses routes
		// it to the ORDER BY position: reached through convertOperands (an
		// OrderBy inside And/Or/Not), GORM's builder inlined "ORDER BY ..." INTO
		// the WHERE clause and handed the caller unexecutable SQL with ok=true.
		// ORDER BY rendering happens in ApplyGorm instead, via
		// gormOrderByClause, from the two positions where a sort spec belongs:
		// the top-level clause list and GetSort.
		return nil, nil
	default:
		return nil, fmt.Errorf("gorm adapter: unsupported expression type %T%s", e, docExprSupport(e))
	}
}

// gormAppliedSetting marks a *gorm.DB that already went through ApplyGorm so
// the adapter never double-applies. A caller-scoped DB (tenant filters etc.)
// does not carry the marker, so figo's filters are applied on top of it.
//
// The stored value is the figo instance that did the applying, not a bare
// `true`: the marker used to record only "some figo configured this DB", so a
// reused handle (the common `base := db.Model(&X{})`) made every LATER instance
// a no-op and serve the FIRST instance's filters — one tenant's query returning
// another tenant's rows, with ok=true and no error.
const gormAppliedSetting = "figo:applied"

// gormAppliedMarker returns the identity value stored under gormAppliedSetting
// for f. Only pointer-shaped instances (figo.New's *figo) have a usable
// identity; anything else is never marked, which degrades to applying twice
// (a duplicated, still-correct predicate) rather than skipping wrongly.
func gormAppliedMarker(f figo.Figo) (any, bool) {
	v := reflect.ValueOf(f)
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		// Storing f itself (not its address as a number) also keeps the
		// instance reachable, so its address cannot be recycled underneath a
		// DB that still carries the marker.
		return f, true
	}
	return nil, false
}

// gormAppliedBy reports whether db was already configured by THIS figo
// instance. reflect is used for the comparison because a value stored under the
// same key by someone else may not be comparable with ==.
func gormAppliedBy(f figo.Figo, db *gorm.DB) bool {
	marker, ok := gormAppliedMarker(f)
	if !ok {
		return false
	}
	stored, found := db.Get(gormAppliedSetting)
	if !found {
		return false
	}
	sv, mv := reflect.ValueOf(stored), reflect.ValueOf(marker)
	return sv.Kind() == reflect.Pointer && mv.Kind() == reflect.Pointer && sv.Pointer() == mv.Pointer()
}

// gormSafeClone returns a handle whose Statement is a private copy, so nothing
// this adapter writes (settings, limit/offset, select, preloads, where clauses,
// sort) lands in the caller's *gorm.DB. GORM's Session clones the Statement
// only when Context/PrepareStmt/SkipHooks is set, so the statement's own
// Context is passed back in to force the copy.
//
// Without it ApplyGorm mutated a chained handle like db.Model(&X{}) in place,
// which is the single root cause of three defects: a second figo instance
// rendering against the same handle inherited the first one's filters and its
// "already applied" marker; a segment render left its fragment in the caller's
// statement buffer; and two goroutines rendering concurrently (documented as
// safe after Build) raced on the shared clause map.
func gormSafeClone(db *gorm.DB) *gorm.DB {
	if db == nil || db.Statement == nil {
		return db
	}
	return db.Session(&gorm.Session{Context: gormStatementContext(db)})
}

// gormStatementContext returns the statement's context, never nil — Session
// only clones the Statement when a non-nil Context is supplied.
func gormStatementContext(db *gorm.DB) context.Context {
	if db != nil && db.Statement != nil && db.Statement.Context != nil {
		return db.Statement.Context
	}
	return context.Background()
}

// gormProjectionName screens one projection name for the SELECT list and
// reports whether it is a BARE column identifier, optionally table-qualified
// ("age", "users.age").
//
// A bare identifier may be handed to GORM verbatim: unicode letters and digits,
// '_', '$' and '.' between non-empty segments carry no whitespace, quote,
// comment, parenthesis, operator or statement separator, so GORM's
// clause.Column{Raw: true} fallback (which is what an entry that does not
// resolve to a schema field renders as) cannot turn it into SQL structure. That
// matters because the schema is not parsed yet when ApplyGorm runs, so the
// adapter cannot tell which names GORM will resolve — and a resolved one is
// quoted anyway.
//
// Anything else is not rejected, it is dialector-quoted by the caller before it
// goes in — so an expression projection such as "COUNT(*)" renders as the column
// `COUNT(*)` and fails with "no such column" rather than executing, which is the
// rendering this adapter has had since the injection was closed.
//
// The error return is reserved for the two classes the raw adapter's
// validateIdent also refuses, because quoting cannot turn either into a column
// reference:
//
//   - a C0/DEL control byte. A NUL makes the rendered statement unparseable, and
//     a driver that hands identifiers to the engine as C strings may instead
//     TRUNCATE the name and read a different column.
//   - an empty name or an empty dot segment ("", "..."). Each renders as a
//     zero-length delimited identifier, i.e. a CONSTANT column: "..." projected
//     `.`.`.` and came back with rows and no error, a silently wrong result set.
func gormProjectionName(name string) (bare bool, err error) {
	if name == "" {
		return false, fmt.Errorf("gorm adapter: select field %q is empty", name)
	}
	for _, seg := range strings.Split(name, ".") {
		if seg == "" {
			return false, fmt.Errorf("gorm adapter: select field %q has an empty name segment", name)
		}
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c == 0x7f {
			return false, fmt.Errorf("gorm adapter: select field %q contains control byte 0x%02x", name, c)
		}
	}
	bare = true
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' && r != '.' {
			bare = false
			break
		}
	}
	return bare, nil
}

// gormQuoteIdent renders name as a delimited identifier using the DB's own
// dialector — the same quoting clause.Column applies. It falls back to the
// unquoted name only when there is no dialector to ask, which is a DB that
// cannot render any SQL at all.
func gormQuoteIdent(db *gorm.DB, name string) string {
	if db == nil || db.Statement == nil || db.Statement.DB == nil || db.Statement.DB.Dialector == nil {
		return name
	}
	return db.Statement.Quote(name)
}

// ApplyGorm applies pagination, preloads, where clauses, and sorting to a GORM DB instance.
// The caller's handle is left untouched: everything is applied to a private
// clone of its statement (see gormSafeClone).
func ApplyGorm(f figo.Figo, trx *gorm.DB) *gorm.DB {
	trx = gormSafeClone(trx)
	if marker, ok := gormAppliedMarker(f); ok {
		trx = trx.Set(gormAppliedSetting, marker)
	}
	// Take/Skip <= 0 mean "no limit"/"no offset" in every other adapter;
	// passing 0 to GORM's Limit would return zero rows instead.
	if take := f.GetPage().Take; take > 0 {
		trx = trx.Limit(take)
	}
	if skip := f.GetPage().Skip; skip > 0 {
		trx = trx.Offset(skip)
	}

	// select fields; keys are sorted so the column list is deterministic (the
	// raw and Elasticsearch adapters sort theirs too), not map-iteration order.
	if sel := f.GetSelectFields(); len(sel) > 0 {
		// The projection goes through Statement.Selects, NOT through a
		// standalone clause.Select: GORM's callbacks.BuildQuerySQL folds Selects
		// INTO the clause.Select it builds itself, and that same clause is the
		// one carrying Statement.Distinct and the columns appended for each
		// Statement.Joins relation. Installing the clause here pre-empted it
		// (AddClauseIfNotExists became a no-op), so a caller's Distinct() was
		// silently dropped — duplicate rows, err=nil — and a caller's
		// Joins("Rel") kept the JOIN but lost the joined columns, scanning the
		// relation back zero-valued. Selects is also order-independent: a caller
		// may chain Distinct()/Joins() AFTER ApplyGorm returns and still get
		// them applied.
		//
		// Feeding Selects is only safe because every name is screened first.
		// GORM renders a Selects entry it cannot resolve to a schema field as
		// clause.Column{Raw: true} — verbatim SQL — so a crafted name like
		// "(SELECT/**/token/**/FROM/**/vaults)AS/**/x" used to execute as an
		// expression and read another table (SQL injection; the naming func
		// blocks spaces but not /**/ comments). See gormProjectionName.
		names := make([]string, 0, len(sel))
		for _, name := range sortedKeys(sel) {
			col := normalizeColumnName(f, name)
			bare, err := gormProjectionName(col)
			if err != nil {
				// Fail closed, exactly like the conversion errors below: an
				// unrenderable projection is an error, never SQL that means
				// something else. GORM's standard callbacks are guarded by
				// db.Error == nil, so nothing executes.
				_ = trx.AddError(fmt.Errorf("figo: %w", err))
				continue
			}
			if !bare {
				// Not a bare identifier, so it must not go in verbatim: hand
				// GORM the dialector-quoted form instead. Raw:true then emits
				// an already-delimited identifier — the same rendering a
				// clause.Column produces, and the same hardening the raw
				// adapter's quoteIdent applies.
				col = gormQuoteIdent(trx, col)
			}
			names = append(names, col)
		}
		if len(names) > 0 {
			trx = trx.Select(names)
		}
	}

	// A conversion error is recorded on the DB via AddError instead of the
	// clause being silently dropped (which would widen the result set). GORM's
	// standard callbacks are guarded by db.Error == nil, so an errored DB
	// never executes the widened query; callers see the error on Find/Error.
	// Preloads are applied in sorted relation order (as the raw and Mongo
	// adapters render theirs) so a conversion failure always surfaces the same
	// error first, rather than whichever one map iteration happened to reach.
	preloadExprs := f.GetPreloads()
	relations := make([]string, 0, len(preloadExprs))
	for relation := range preloadExprs {
		relations = append(relations, relation)
	}
	sortStrings(relations)

	for _, k := range relations {
		v := preloadExprs[k]
		var conv []clause.Expression
		for _, e := range v {
			if e == nil {
				continue
			}
			ce, err := toGormClauseWithFigo(e, f)
			if err != nil {
				_ = trx.AddError(fmt.Errorf("figo: %w", err))
				continue
			}
			if ce == nil {
				continue
			}
			conv = append(conv, ce)
		}
		trx = trx.Preload(k, conv)
	}

	if clauses := f.GetClauses(); len(clauses) > 0 {
		var conv []clause.Expression
		for _, e := range clauses {
			if e == nil {
				continue
			}
			if ob, ok := e.(figo.OrderBy); ok {
				// A sort spec in the TOP-LEVEL clause list (reached via
				// AddFilter(figo.OrderBy{}), which figo's normalizeExprFields
				// explicitly supports) renders as ORDER BY here, in the clause
				// list's own order and ahead of GetSort's columns — the same
				// contract the raw adapter's buildOrderBy implements. Ignoring
				// it made ONE figo instance return a different row ORDER on the
				// GORM adapter than on the raw adapter, and with figo's default
				// take:20 in force that is a different PAGE OF ROWS.
				//
				// This is not the expression position: an OrderBy nested inside
				// And/Or/Not still renders as nothing (see the figo.OrderBy case
				// in toGormClauseWithFigo), which is what stops it being inlined
				// into the WHERE clause as an unexecutable "AND `t`.`age` DESC".
				if ce := gormOrderByClause(ob); ce != nil {
					trx = trx.Clauses(ce)
				}
				continue
			}
			ce, err := toGormClauseWithFigo(e, f)
			if err != nil {
				_ = trx.AddError(fmt.Errorf("figo: %w", err))
				continue
			}
			if ce == nil {
				continue
			}
			conv = append(conv, ce)
		}
		trx = trx.Clauses(conv...)
	}

	// Access sort using GetSort method. This is the ONLY position where a sort
	// spec may render, which is why it does not go through the expression
	// converter (see the figo.OrderBy case there).
	sort := f.GetSort()
	if sort != nil {
		if ce := gormOrderByClause(*sort); ce != nil {
			trx = trx.Clauses(ce)
		}
	}

	return trx
}

// gormOrderByClause renders a sort spec as GORM's ORDER BY clause. Used only
// from ApplyGorm, where trx.Clauses routes a clause.OrderBy to the ORDER BY
// position.
func gormOrderByClause(x figo.OrderBy) clause.Expression {
	var cols []clause.OrderByColumn
	for _, c := range x.Columns {
		// Defensively skip empty column names: they would render ORDER BY ``.
		if c.Name == "" {
			continue
		}
		// c.Name was normalized at parse time (see toGormClauseWithFigo).
		col := clause.Column{Name: c.Name, Table: clause.CurrentTable}
		if strings.Contains(c.Name, ".") {
			// An already-qualified sort key carries its own table, so adding
			// CurrentTable prepended a THIRD qualifier and produced
			// `t`.`t`.`age` — unexecutable SQL returned with ok=true. The
			// dialector splits a dotted name into `t`.`age` on its own.
			col.Table = ""
		}
		cols = append(cols, clause.OrderByColumn{Column: col, Desc: c.Desc})
	}
	if len(cols) == 0 {
		return nil
	}
	return clause.OrderBy{Columns: cols}
}

// gormDryRunSQL renders the statement for a configured GORM DB via DryRun and
// returns the placeholder SQL plus its bind variables. With no conditionType
// it renders the complete SELECT the way GORM itself would execute it; with
// segment names ("WHERE", "SORT", ...) it builds only those clauses.
func gormDryRunSQL(trx *gorm.DB, conditionType ...string) (string, []any) {
	if len(conditionType) == 0 {
		// A chained call (Find) resets a NewDB session's statement, so keep
		// the statement and let GORM's own query path build the full SELECT.
		tr := trx.Session(&gorm.Session{DryRun: true})
		tr.Logger = logger.Default.LogMode(logger.Silent)
		dest := tr.Statement.Dest
		if dest == nil {
			if tr.Statement.Model != nil {
				dest = tr.Statement.Model
			} else {
				dest = &[]map[string]any{}
			}
		}
		tr = tr.Find(dest)
		return tr.Statement.SQL.String(), tr.Statement.Vars
	}

	// Context is passed so Session actually CLONES the statement: it clones
	// only when Context/PrepareStmt/SkipHooks is set, so without it tr.Statement
	// ALIASES the caller's, and the fragment built below was left in their
	// buffer — their next Find then failed with `near "WHERE": syntax error`.
	tr := trx.Session(&gorm.Session{DryRun: true, NewDB: true, Context: gormStatementContext(trx)})
	tr.Logger = logger.Default.LogMode(logger.Silent)

	stmt := tr.Statement

	tr.Callback().Query().Execute(tr)
	// When the caller's DB was opened with a global DryRun:true, Execute
	// leaves the fully built SELECT in the statement buffer, so Build would
	// APPEND the requested segment to it — and the segment's binds to Vars,
	// duplicating every arg. Build renders from stmt.Clauses, so starting it
	// from a clean buffer is correct under every caller configuration.
	stmt.SQL.Reset()
	stmt.Vars = nil
	stmt.Build(conditionType...)
	return stmt.SQL.String(), stmt.Vars
}

// getGormSqlString renders the SQL string from a configured GORM DB instance
// using DryRun, with placeholders interpolated for display.
func getGormSqlString(trx *gorm.DB, conditionType ...string) string {
	sqlWithPlaceholders, params := gormDryRunSQL(trx, conditionType...)
	return trx.Dialector.Explain(sqlWithPlaceholders, params...)
}

// applyGormOnce applies figo's state to the DB unless THIS instance already
// applied it (the caller may pre-apply and pass the result back in). A DB
// configured by a different figo instance is not skipped — that is what leaked
// one instance's filters into another's query.
func applyGormOnce(f figo.Figo, db *gorm.DB) *gorm.DB {
	if gormAppliedBy(f, db) {
		return db
	}
	return ApplyGorm(f, db)
}

// AdapterGormGetSql is an internal helper to integrate with figo.GetSqlString.
// If applying figo's state recorded an error on the DB (e.g. an expression
// the GORM adapter cannot render), the render fails (ok=false) — fail closed
// rather than returning SQL that omits a predicate.
func AdapterGormGetSql(f figo.Figo, ctx any, conditionType ...string) (string, bool) {
	db, ok := ctx.(*gorm.DB)
	if !ok || db == nil {
		return "", false
	}
	applied := applyGormOnce(f, db)
	if applied.Error != nil {
		return "", false
	}
	return getGormSqlString(applied, conditionType...), true
}

// GormAdapter is an Adapter object you can pass to Build
type GormAdapter struct{}

func (GormAdapter) GetSqlString(f figo.Figo, ctx any, conditionType ...string) (string, bool) {
	if f == nil {
		return "", false
	}
	return AdapterGormGetSql(f, ctx, conditionType...)
}

func (GormAdapter) GetQuery(f figo.Figo, ctx any, conditionType ...string) (figo.Query, bool) {
	if f == nil {
		return nil, false
	}
	db, ok := ctx.(*gorm.DB)
	if !ok || db == nil {
		return nil, false
	}
	applied := applyGormOnce(f, db)
	if applied.Error != nil {
		// See AdapterGormGetSql: fail closed on conversion errors.
		return nil, false
	}
	sql, args := gormDryRunSQL(applied, conditionType...)
	return figo.SQLQuery{SQL: sql, Args: args}, true
}

// gormRawBytes carries a byte slice through GORM's placeholder expansion
// intact. GORM's clause.Expr splits ANY slice bound to a '?' that follows '('
// into one placeholder per element (clause/expression.go processValue), and it
// exempts only driver.Valuer — so a CustomExpr handler returning
// `hash IN (?)` with a []byte blob rendered `hash IN (?,?)` bound to the
// individual BYTES, silently matching nothing while the raw adapter, which
// documents the same handler contract, bound the blob and matched the row.
type gormRawBytes []byte

// Value implements driver.Valuer, which is what makes GORM leave it alone.
func (b gormRawBytes) Value() (driver.Value, error) { return []byte(b), nil }

// gormProtectByteSlices wraps byte-slice args so GORM binds them as blobs.
// Other slice types are deliberately left expandable: that is GORM's rule for
// `IN (?)` and the raw adapter mirrors it in expandSliceArgs.
func gormProtectByteSlices(args []any) []any {
	var out []any
	for i, a := range args {
		if a == nil {
			continue
		}
		if _, ok := a.(driver.Valuer); ok {
			continue
		}
		rv := reflect.ValueOf(a)
		if rv.Kind() != reflect.Slice || rv.Type().Elem().Kind() != reflect.Uint8 {
			continue
		}
		if out == nil {
			out = append(out, args...)
		}
		out[i] = gormRawBytes(rv.Bytes())
	}
	if out == nil {
		return args
	}
	return out
}
