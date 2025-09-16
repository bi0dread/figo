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
		return clause.IN{Column: getFieldName(x.Field), Values: x.Values}
	case NotInExpr:
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

// ApplyGorm applies pagination, preloads, where clauses, and sorting to a GORM DB instance.
func ApplyGorm(f Figo, trx *gorm.DB) *gorm.DB {
	trx = trx.Limit(f.GetPage().Take)
	trx = trx.Offset(f.GetPage().Skip)

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

// GetGormSqlString renders the SQL string from a configured GORM DB instance using DryRun.
func getGormSqlString(trx *gorm.DB, conditionType ...string) string {
	tr := trx.Begin()
	if tr.Error != nil {
		// If Begin fails, use the original DB with dry run
		tr = trx.Session(&gorm.Session{DryRun: true, NewDB: true})
	} else {
		tr = tr.Session(&gorm.Session{DryRun: true, NewDB: true})
	}
	tr.Logger = logger.Default.LogMode(logger.Silent)

	stmt := tr.Statement

	tr.Callback().Query().Execute(tr)
	stmt.Build(conditionType...)
	sqlWithPlaceholders := stmt.SQL.String()
	params := stmt.Vars

	fullSQL := tr.Dialector.Explain(sqlWithPlaceholders, params...)

	// Only rollback if we successfully began a transaction
	if tr.Error == nil {
		tr.Rollback()
	}

	return fullSQL
}

// AdapterGormGetSql is an internal helper to integrate with figo.GetSqlString
func AdapterGormGetSql(f Figo, ctx any, conditionType ...string) (string, bool) {
	db, ok := ctx.(*gorm.DB)
	if !ok || db == nil {
		return "", false
	}
	// Check if the DB already has clauses applied (to avoid double-applying)
	// If it has clauses, use it directly; otherwise apply Figo filters
	if len(db.Statement.Clauses) > 0 {
		// DB already has clauses applied, use it directly
		return getGormSqlString(db, conditionType...), true
	} else {
		// Apply Figo filters and sort to the GORM DB instance first
		appliedDB := ApplyGorm(f, db)
		return getGormSqlString(appliedDB, conditionType...), true
	}
}

// GormAdapter is an Adapter object you can pass to NewWithAdapterObject
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
	sql := getGormSqlString(db, conditionType...)
	return SQLQuery{SQL: sql, Args: nil}, true
}
