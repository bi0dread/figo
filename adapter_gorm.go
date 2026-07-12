package figo

import (
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// toGormClause converts an internal Expr tree into a gorm clause.Expression
func toGormClause(e Expr) clause.Expression {
	return toGormClauseWithFigo(e, nil)
}

// toGormClauseWithFigo converts an internal Expr tree into a gorm clause.Expression with field name normalization
func toGormClauseWithFigo(e Expr, f Figo) clause.Expression {
	// Helper function to get normalized field name
	getFieldName := func(field string) string {
		if f != nil {
			return normalizeColumnName(f, field)
		}
		return field
	}

	switch x := e.(type) {
	case EqExpr:
		return clause.Eq{Column: getFieldName(x.Field), Value: x.Value}
	case GteExpr:
		return clause.Gte{Column: getFieldName(x.Field), Value: x.Value}
	case GtExpr:
		return clause.Gt{Column: getFieldName(x.Field), Value: x.Value}
	case LtExpr:
		return clause.Lt{Column: getFieldName(x.Field), Value: x.Value}
	case LteExpr:
		return clause.Lte{Column: getFieldName(x.Field), Value: x.Value}
	case NeqExpr:
		return clause.Neq{Column: getFieldName(x.Field), Value: x.Value}
	case LikeExpr:
		return clause.Like{Column: getFieldName(x.Field), Value: x.Value}
	case RegexExpr:
		// Use configurable regex operator; default REGEXP, set to ~ or ~* for Postgres.
		return clause.Expr{SQL: fmt.Sprintf("? %s ?", GetRegexSQLOperator()), Vars: []any{clause.Column{Name: getFieldName(x.Field)}, x.Value}}
	case ILikeExpr:
		// GORM has no ILIKE portable operator; fallback to LOWER(col) LIKE LOWER(?)
		return clause.Expr{SQL: "LOWER(?) LIKE LOWER(?)", Vars: []any{clause.Column{Name: getFieldName(x.Field)}, x.Value}}
	case IsNullExpr:
		return clause.Eq{Column: getFieldName(x.Field), Value: nil}
	case NotNullExpr:
		return clause.Neq{Column: getFieldName(x.Field), Value: nil}
	case InExpr:
		if len(x.Values) == 0 {
			// Empty IN matches nothing. Mirror the raw adapter's 1=0 rather than
			// relying on GORM's clause.IN, which for zero values emits "IN (NULL)".
			return clause.Expr{SQL: "1=0"}
		}
		return clause.IN{Column: getFieldName(x.Field), Values: x.Values}
	case NotInExpr:
		if len(x.Values) == 0 {
			// "NOT IN (empty set)" is true for every row. GORM's
			// clause.Not(clause.IN{}) instead emits "col IS NOT NULL", which
			// wrongly excludes NULL rows and diverges from the raw adapter's 1=1.
			return clause.Expr{SQL: "1=1"}
		}
		return clause.Not(clause.IN{Column: getFieldName(x.Field), Values: x.Values})
	case BetweenExpr:
		return clause.Expr{SQL: "? BETWEEN ? AND ?", Vars: []any{clause.Column{Name: getFieldName(x.Field)}, x.Low, x.High}}
	case AndExpr:
		var parts []clause.Expression
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			parts = append(parts, toGormClauseWithFigo(op, f))
		}
		return clause.And(parts...)
	case OrExpr:
		var parts []clause.Expression
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			parts = append(parts, toGormClauseWithFigo(op, f))
		}
		return clause.Or(parts...)
	case NotExpr:
		var parts []clause.Expression
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			parts = append(parts, toGormClauseWithFigo(op, f))
		}
		return clause.Not(parts...)
	case OrderBy:
		var cols []clause.OrderByColumn
		for _, c := range x.Columns {
			normalizedName := getFieldName(c.Name)
			cols = append(cols, clause.OrderByColumn{Column: clause.Column{Name: normalizedName, Table: clause.CurrentTable}, Desc: c.Desc})
		}
		return clause.OrderBy{Columns: cols}
	default:
		return nil
	}
}

// gormAppliedSetting marks a *gorm.DB that already went through ApplyGorm so
// the adapter never double-applies. A caller-scoped DB (tenant filters etc.)
// does not carry the marker, so figo's filters are applied on top of it.
const gormAppliedSetting = "figo:applied"

// ApplyGorm applies pagination, preloads, where clauses, and sorting to a GORM DB instance.
func ApplyGorm(f Figo, trx *gorm.DB) *gorm.DB {
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

	for k, v := range f.GetPreloads() {
		var conv []clause.Expression
		for _, e := range v {
			if e == nil {
				continue
			}
			conv = append(conv, toGormClauseWithFigo(e, f))
		}
		trx = trx.Preload(k, conv)
	}

	if clauses := f.GetClauses(); len(clauses) > 0 {
		var conv []clause.Expression
		for _, e := range clauses {
			if e == nil {
				continue
			}
			conv = append(conv, toGormClauseWithFigo(e, f))
		}
		trx = trx.Clauses(conv...)
	}

	// Access sort using GetSort method
	sort := f.GetSort()
	if sort != nil {
		trx = trx.Clauses(toGormClauseWithFigo(*sort, f))
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
func applyGormOnce(f Figo, db *gorm.DB) *gorm.DB {
	if _, applied := db.Get(gormAppliedSetting); applied {
		return db
	}
	return ApplyGorm(f, db)
}

// AdapterGormGetSql is an internal helper to integrate with figo.GetSqlString
func AdapterGormGetSql(f Figo, ctx any, conditionType ...string) (string, bool) {
	db, ok := ctx.(*gorm.DB)
	if !ok || db == nil {
		return "", false
	}
	return getGormSqlString(applyGormOnce(f, db), conditionType...), true
}

// GormAdapter is an Adapter object you can pass to Build
type GormAdapter struct{}

func (GormAdapter) GetSqlString(f Figo, ctx any, conditionType ...string) (string, bool) {
	if f == nil {
		return "", false
	}
	return AdapterGormGetSql(f, ctx, conditionType...)
}

func (GormAdapter) GetQuery(f Figo, ctx any, conditionType ...string) (Query, bool) {
	if f == nil {
		return nil, false
	}
	db, ok := ctx.(*gorm.DB)
	if !ok || db == nil {
		return nil, false
	}
	sql, args := gormDryRunSQL(applyGormOnce(f, db), conditionType...)
	return SQLQuery{SQL: sql, Args: args}, true
}
