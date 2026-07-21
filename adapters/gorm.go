package adapters

import (
	figo "github.com/bi0dread/figo/v4"

	"fmt"

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
			// empty-IN guard above closes and that Mongo ($nor:[{}]) and ES
			// (empty should) already prevent.
			return clause.Expr{SQL: "1=0"}, nil
		}
		return clause.Or(parts...), nil
	case figo.NotExpr:
		parts, err := convertOperands(x.Operands)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			// NOT of nothing is vacuously true: no predicate, like Mongo's
			// empty $nor.
			return nil, nil
		}
		return clause.Not(parts...), nil
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
		return clause.Expr{SQL: frag, Vars: args}, nil
	case figo.OrderBy:
		var cols []clause.OrderByColumn
		for _, c := range x.Columns {
			// c.Name was normalized at parse time (see getFieldName above).
			cols = append(cols, clause.OrderByColumn{Column: clause.Column{Name: c.Name, Table: clause.CurrentTable}, Desc: c.Desc})
		}
		return clause.OrderBy{Columns: cols}, nil
	default:
		return nil, fmt.Errorf("gorm adapter: unsupported expression type %T (rendered by the MongoDB/Elasticsearch adapters only)", e)
	}
}

// gormAppliedSetting marks a *gorm.DB that already went through ApplyGorm so
// the adapter never double-applies. A caller-scoped DB (tenant filters etc.)
// does not carry the marker, so figo's filters are applied on top of it.
const gormAppliedSetting = "figo:applied"

// ApplyGorm applies pagination, preloads, where clauses, and sorting to a GORM DB instance.
func ApplyGorm(f figo.Figo, trx *gorm.DB) *gorm.DB {
	trx = trx.Set(gormAppliedSetting, true)
	// Take/Skip <= 0 mean "no limit"/"no offset" in every other adapter;
	// passing 0 to GORM's Limit would return zero rows instead.
	if take := f.GetPage().Take; take > 0 {
		trx = trx.Limit(take)
	}
	if skip := f.GetPage().Skip; skip > 0 {
		trx = trx.Offset(skip)
	}

	// select fields
	if len(f.GetSelectFields()) > 0 {
		fields := make([]string, 0, len(f.GetSelectFields()))
		for name := range f.GetSelectFields() {
			fields = append(fields, normalizeColumnName(f, name))
		}
		trx = trx.Select(fields)
	}

	// A conversion error is recorded on the DB via AddError instead of the
	// clause being silently dropped (which would widen the result set). GORM's
	// standard callbacks are guarded by db.Error == nil, so an errored DB
	// never executes the widened query; callers see the error on Find/Error.
	for k, v := range f.GetPreloads() {
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

	// Access sort using GetSort method
	sort := f.GetSort()
	if sort != nil {
		if ce, err := toGormClauseWithFigo(*sort, f); err != nil {
			_ = trx.AddError(fmt.Errorf("figo: %w", err))
		} else if ce != nil {
			trx = trx.Clauses(ce)
		}
	}

	return trx
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

	tr := trx.Session(&gorm.Session{DryRun: true, NewDB: true})
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

// applyGormOnce applies figo's state to the DB unless it already went through
// ApplyGorm (the caller may pre-apply and pass the result back in).
func applyGormOnce(f figo.Figo, db *gorm.DB) *gorm.DB {
	if _, applied := db.Get(gormAppliedSetting); applied {
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
