package figo

import (
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// toGormClause converts an internal Expr tree into a gorm clause.Expression
func toGormClause(e Expr) clause.Expression {
	switch x := e.(type) {
	case EqExpr:
		return clause.Eq{Column: x.Field, Value: x.Value}
	case GteExpr:
		return clause.Gte{Column: x.Field, Value: x.Value}
	case GtExpr:
		return clause.Gt{Column: x.Field, Value: x.Value}
	case LtExpr:
		return clause.Lt{Column: x.Field, Value: x.Value}
	case LteExpr:
		return clause.Lte{Column: x.Field, Value: x.Value}
	case NeqExpr:
		return clause.Neq{Column: x.Field, Value: x.Value}
	case LikeExpr:
		return clause.Like{Column: x.Field, Value: x.Value}
	case RegexExpr:
		// Use configurable regex operator; default REGEXP, set to ~ or ~* for Postgres.
		return clause.Expr{SQL: fmt.Sprintf("? %s ?", GetRegexSQLOperator()), Vars: []any{clause.Column{Name: x.Field}, x.Value}}
	case ILikeExpr:
		// GORM has no ILIKE portable operator; fallback to LOWER(col) LIKE LOWER(?)
		return clause.Expr{SQL: "LOWER(?) LIKE LOWER(?)", Vars: []any{clause.Column{Name: x.Field}, x.Value}}
	case IsNullExpr:
		return clause.Eq{Column: x.Field, Value: nil}
	case NotNullExpr:
		return clause.Neq{Column: x.Field, Value: nil}
	case InExpr:
		return clause.IN{Column: x.Field, Values: x.Values}
	case NotInExpr:
		return clause.Not(clause.IN{Column: x.Field, Values: x.Values})
	case BetweenExpr:
		return clause.Expr{SQL: "? BETWEEN ? AND ?", Vars: []any{clause.Column{Name: x.Field}, x.Low, x.High}}
	case AndExpr:
		var parts []clause.Expression
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			parts = append(parts, toGormClause(op))
		}
		return clause.And(parts...)
	case OrExpr:
		var parts []clause.Expression
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			parts = append(parts, toGormClause(op))
		}
		return clause.Or(parts...)
	case NotExpr:
		var parts []clause.Expression
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			parts = append(parts, toGormClause(op))
		}
		return clause.Not(parts...)
	case OrderBy:
		var cols []clause.OrderByColumn
		for _, c := range x.Columns {
			cols = append(cols, clause.OrderByColumn{Column: clause.Column{Name: c.Name, Table: clause.CurrentTable}, Desc: c.Desc})
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
			conv = append(conv, toGormClause(e))
		}
		trx = trx.Preload(k, conv)
	}

	if clauses := f.GetClauses(); len(clauses) > 0 {
		var conv []clause.Expression
		for _, e := range clauses {
			if e == nil {
				continue
			}
			conv = append(conv, toGormClause(e))
		}
		trx = trx.Clauses(conv...)
	}

	// Access internal sort if available
	if x, ok := f.(*figo); ok && x.sort != nil {
		trx = trx.Clauses(toGormClause(*x.sort))
	}

	return trx
}

// GetGormSqlString renders the SQL string from a configured GORM DB instance using DryRun.
func getGormSqlString(trx *gorm.DB, conditionType ...string) string {
	tr := trx.Begin()
	tr = tr.Session(&gorm.Session{DryRun: true, NewDB: true})
	tr.Logger = logger.Default.LogMode(logger.Silent)

	stmt := tr.Statement

	tr.Callback().Query().Execute(tr)
	stmt.Build(conditionType...)
	sqlWithPlaceholders := stmt.SQL.String()
	params := stmt.Vars

	fullSQL := tr.Dialector.Explain(sqlWithPlaceholders, params...)

	tr.Rollback()

	return fullSQL
}

// AdapterGormGetSql is an internal helper to integrate with figo.GetSqlString
func AdapterGormGetSql(_ Figo, ctx any, conditionType ...string) (string, bool) {
	db, ok := ctx.(*gorm.DB)
	if !ok || db == nil {
		return "", false
	}
	return getGormSqlString(db, conditionType...), true
}

// GormAdapter is an Adapter object you can pass to NewWithAdapterObject
type GormAdapter struct{}

func (GormAdapter) GetSqlString(f Figo, ctx any, conditionType ...string) (string, bool) {
	return AdapterGormGetSql(f, ctx, conditionType...)
}

func (GormAdapter) GetQuery(f Figo, ctx any, conditionType ...string) (Query, bool) {
	db, ok := ctx.(*gorm.DB)
	if !ok || db == nil {
		return nil, false
	}
	sql := getGormSqlString(db, conditionType...)
	return SQLQuery{SQL: sql, Args: nil}, true
}
