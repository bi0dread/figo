package figo

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gobeam/stringy"
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

// RawPreload represents a built WHERE clause and args for a preload relationship
type RawPreload struct {
	Where string
	Args  []any
}

// BuildRawPreloads builds WHERE clauses for each preload relationship without any ORM dependency
func BuildRawPreloads(f Figo) map[string]RawPreload {
	result := make(map[string]RawPreload)
	for rel, exprs := range f.GetPreloads() {
		where, args := buildWhereFromExprs(exprs)
		result[rel] = RawPreload{Where: where, Args: args}
	}
	return result
}

// BuildRawWhere builds a SQL WHERE clause (without the leading WHERE keyword) and its args
func BuildRawWhere(f Figo) (string, []any) {
	return buildWhereFromExprs(f.GetClauses())
}

// BuildRawSelect builds a full SELECT query for the given table and columns.
// Identifiers are quoted with backticks to be broadly compatible with MySQL-like dialects.
// Placeholders use the '?' style.
func BuildRawSelect(f Figo, table string, columns ...string) (string, []any) {
	cols := "*"
	// prefer selectFields if provided
	if sel := f.GetSelectFields(); len(sel) > 0 {
		quoted := make([]string, 0, len(sel))
		for _, name := range sortedKeys(sel) {
			quoted = append(quoted, quoteIdent(normalizeColumnName(f, name)))
		}
		cols = strings.Join(quoted, ", ")
	} else if len(columns) > 0 {
		quoted := make([]string, 0, len(columns))
		for _, c := range columns {
			quoted = append(quoted, quoteIdent(c))
		}
		cols = strings.Join(quoted, ", ")
	}

	// joins must come before WHERE and their args first
	joinSQL, joinArgs := buildJoins(f)
	where, whereArgs := BuildRawWhere(f)
	orderBy := buildOrderBy(f)
	limitOffset := buildLimitOffset(f)

	query := fmt.Sprintf("SELECT %s FROM %s", cols, quoteIdent(table))
	if joinSQL != "" {
		query += " " + joinSQL
	}
	if where != "" {
		query += " WHERE " + where
	}
	if orderBy != "" {
		query += " " + orderBy
	}
	if limitOffset != "" {
		query += " " + limitOffset
	}
	// args: first joins, then where
	args := append([]any{}, joinArgs...)
	args = append(args, whereArgs...)
	return query, args
}

// -- internals --

func buildWhereFromExprs(exprs []Expr) (string, []any) {
	if len(exprs) == 0 {
		return "", nil
	}

	// If multiple top-level expressions exist, combine with AND
	parts := make([]string, 0, len(exprs))
	args := make([]any, 0)
	for _, e := range exprs {
		if e == nil {
			continue
		}
		p, a := exprToSQL(e)
		if p != "" {
			parts = append(parts, p)
			args = append(args, a...)
		}
	}
	return strings.Join(parts, " AND "), args
}

func exprToSQL(e Expr) (string, []any) {
	switch x := e.(type) {
	case EqExpr:
		return fmt.Sprintf("%s = ?", quoteIdent(x.Field)), []any{x.Value}
	case GteExpr:
		return fmt.Sprintf("%s >= ?", quoteIdent(x.Field)), []any{x.Value}
	case GtExpr:
		return fmt.Sprintf("%s > ?", quoteIdent(x.Field)), []any{x.Value}
	case LtExpr:
		return fmt.Sprintf("%s < ?", quoteIdent(x.Field)), []any{x.Value}
	case LteExpr:
		return fmt.Sprintf("%s <= ?", quoteIdent(x.Field)), []any{x.Value}
	case NeqExpr:
		return fmt.Sprintf("%s != ?", quoteIdent(x.Field)), []any{x.Value}
	case LikeExpr:
		return fmt.Sprintf("%s LIKE ?", quoteIdent(x.Field)), []any{x.Value}
	case RegexExpr:
		// Use configurable operator (default REGEXP). For Postgres, set to ~ or ~* via SetRegexSQLOperator
		return fmt.Sprintf("%s %s ?", quoteIdent(x.Field), GetRegexSQLOperator()), []any{x.Value}
	case ILikeExpr:
		return fmt.Sprintf("LOWER(%s) LIKE LOWER(?)", quoteIdent(x.Field)), []any{x.Value}
	case IsNullExpr:
		return fmt.Sprintf("%s IS NULL", quoteIdent(x.Field)), nil
	case NotNullExpr:
		return fmt.Sprintf("%s IS NOT NULL", quoteIdent(x.Field)), nil
	case InExpr:
		if len(x.Values) == 0 {
			// An empty IN set matches nothing. Returning "" would drop the whole
			// predicate (WHERE disappears), turning a match-nothing filter into a
			// match-everything one — a filter/authorization bypass.
			return "1=0", nil
		}
		placeholders := strings.Repeat("?,", len(x.Values))
		placeholders = placeholders[:len(placeholders)-1]
		return fmt.Sprintf("%s IN (%s)", quoteIdent(x.Field), placeholders), append([]any{}, x.Values...)
	case NotInExpr:
		if len(x.Values) == 0 {
			// "NOT IN (empty set)" is true for every row.
			return "1=1", nil
		}
		placeholders := strings.Repeat("?,", len(x.Values))
		placeholders = placeholders[:len(placeholders)-1]
		return fmt.Sprintf("%s NOT IN (%s)", quoteIdent(x.Field), placeholders), append([]any{}, x.Values...)
	case BetweenExpr:
		return fmt.Sprintf("%s BETWEEN ? AND ?", quoteIdent(x.Field)), []any{x.Low, x.High}
	case AndExpr:
		return joinGroup("AND", x.Operands)
	case OrExpr:
		return joinGroup("OR", x.Operands)
	case NotExpr:
		if len(x.Operands) == 0 {
			return "", nil
		}
		inner, args := joinGroup("AND", x.Operands)
		if inner == "" {
			return "", nil
		}
		return fmt.Sprintf("NOT (%s)", inner), args
	case OrderBy:
		// handled separately in buildOrderBy
		return "", nil
	default:
		return "", nil
	}
}

func exprToSQLQualified(e Expr, qualifier string) (string, []any) {
	// Qualify column references with the given table name
	qcol := func(field string) string { return quoteIdent(qualifier) + "." + quoteIdent(field) }
	switch x := e.(type) {
	case EqExpr:
		return fmt.Sprintf("%s = ?", qcol(x.Field)), []any{x.Value}
	case GteExpr:
		return fmt.Sprintf("%s >= ?", qcol(x.Field)), []any{x.Value}
	case GtExpr:
		return fmt.Sprintf("%s > ?", qcol(x.Field)), []any{x.Value}
	case LtExpr:
		return fmt.Sprintf("%s < ?", qcol(x.Field)), []any{x.Value}
	case LteExpr:
		return fmt.Sprintf("%s <= ?", qcol(x.Field)), []any{x.Value}
	case NeqExpr:
		return fmt.Sprintf("%s != ?", qcol(x.Field)), []any{x.Value}
	case LikeExpr:
		return fmt.Sprintf("%s LIKE ?", qcol(x.Field)), []any{x.Value}
	case RegexExpr:
		return fmt.Sprintf("%s %s ?", qcol(x.Field), GetRegexSQLOperator()), []any{x.Value}
	case ILikeExpr:
		return fmt.Sprintf("LOWER(%s) LIKE LOWER(?)", qcol(x.Field)), []any{x.Value}
	case IsNullExpr:
		return fmt.Sprintf("%s IS NULL", qcol(x.Field)), nil
	case NotNullExpr:
		return fmt.Sprintf("%s IS NOT NULL", qcol(x.Field)), nil
	case InExpr:
		if len(x.Values) == 0 {
			// Empty IN set matches nothing; see exprToSQL for rationale.
			return "1=0", nil
		}
		placeholders := strings.Repeat("?,", len(x.Values))
		placeholders = placeholders[:len(placeholders)-1]
		return fmt.Sprintf("%s IN (%s)", qcol(x.Field), placeholders), append([]any{}, x.Values...)
	case NotInExpr:
		if len(x.Values) == 0 {
			// "NOT IN (empty set)" is true for every row.
			return "1=1", nil
		}
		placeholders := strings.Repeat("?,", len(x.Values))
		placeholders = placeholders[:len(placeholders)-1]
		return fmt.Sprintf("%s NOT IN (%s)", qcol(x.Field), placeholders), append([]any{}, x.Values...)
	case BetweenExpr:
		return fmt.Sprintf("%s BETWEEN ? AND ?", qcol(x.Field)), []any{x.Low, x.High}
	case AndExpr:
		return joinGroupQualified("AND", x.Operands, qualifier)
	case OrExpr:
		return joinGroupQualified("OR", x.Operands, qualifier)
	case NotExpr:
		if len(x.Operands) == 0 {
			return "", nil
		}
		inner, args := joinGroupQualified("AND", x.Operands, qualifier)
		if inner == "" {
			return "", nil
		}
		return fmt.Sprintf("NOT (%s)", inner), args
	case OrderBy:
		return "", nil
	default:
		return "", nil
	}
}

func joinGroup(op string, operands []Expr) (string, []any) {
	parts := make([]string, 0, len(operands))
	args := make([]any, 0)
	for _, e := range operands {
		if e == nil {
			continue
		}
		p, a := exprToSQL(e)
		if p != "" {
			parts = append(parts, p)
			args = append(args, a...)
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	if len(parts) == 1 {
		return parts[0], args
	}
	return "(" + strings.Join(parts, " "+op+" ") + ")", args
}

func joinGroupQualified(op string, operands []Expr, qualifier string) (string, []any) {
	parts := make([]string, 0, len(operands))
	args := make([]any, 0)
	for _, e := range operands {
		if e == nil {
			continue
		}
		p, a := exprToSQLQualified(e, qualifier)
		if p != "" {
			parts = append(parts, p)
			args = append(args, a...)
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	if len(parts) == 1 {
		return parts[0], args
	}
	return "(" + strings.Join(parts, " "+op+" ") + ")", args
}

func buildOrderBy(f Figo) string {
	sort := f.GetSort()
	if sort != nil {
		cols := make([]string, 0, len(sort.Columns))
		for _, c := range sort.Columns {
			dir := "ASC"
			if c.Desc {
				dir = "DESC"
			}
			cols = append(cols, fmt.Sprintf("%s %s", quoteIdent(c.Name), dir))
		}
		if len(cols) > 0 {
			return "ORDER BY " + strings.Join(cols, ", ")
		}
	}
	return ""
}

func buildLimitOffset(f Figo) string {
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
	// OFFSET without LIMIT is a syntax error on MySQL/SQLite, so pair it with the
	// conventional "unbounded" LIMIT.
	return fmt.Sprintf("LIMIT 18446744073709551615 OFFSET %d", p.Skip)
}

func quoteIdent(ident string) string {
	// Escape embedded backticks by doubling them (MySQL/SQLite identifier rule).
	// Without this, a crafted field/table name can break out of the quoting and
	// inject SQL — and identifiers can't be parametrized, so this is the only
	// defense on both the GetSqlString and GetQuery paths.
	return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
}

// AdapterRawGetSql is an internal helper to integrate with figo.GetSqlString
// ctx can be a table name string or a struct containing Table.
type RawContext struct {
	Table string
}

func AdapterRawGetSql(f Figo, ctx any, conditionType ...string) (string, []any, bool) {
	switch v := ctx.(type) {
	case string:
		sql, args := buildByConditions(f, v, conditionType...)
		return sql, args, true
	case RawContext:
		sql, args := buildByConditions(f, v.Table, conditionType...)
		return sql, args, true
	default:
		return "", nil, false
	}
}

// RawAdapter is an Adapter object you can pass to NewWithAdapterObject
type RawAdapter struct{}

func (RawAdapter) GetSqlString(f Figo, ctx any, conditionType ...string) (string, bool) {
	if f == nil {
		return "", false
	}
	sql, args, ok := AdapterRawGetSql(f, ctx, conditionType...)
	if !ok {
		return "", false
	}
	return expandPlaceholders(sql, args), true
}

func (RawAdapter) GetQuery(f Figo, ctx any, conditionType ...string) (Query, bool) {
	if f == nil {
		return nil, false
	}
	sql, args, ok := AdapterRawGetSql(f, ctx, conditionType...)
	if !ok {
		return nil, false
	}
	return SQLQuery{SQL: sql, Args: args}, true
}

func buildByConditions(f Figo, table string, conditionType ...string) (string, []any) {
	// Determine columns from selectFields; default to *
	cols := "*"
	if sel := f.GetSelectFields(); len(sel) > 0 {
		quoted := make([]string, 0, len(sel))
		for _, name := range sortedKeys(sel) {
			quoted = append(quoted, quoteIdent(normalizeColumnName(f, name)))
		}
		cols = strings.Join(quoted, ", ")
	}

	joinSQL, joinArgs := buildJoins(f)
	where, whereArgs := BuildRawWhere(f)
	orderBy := buildOrderBy(f)
	limitOffset := buildLimitOffset(f)

	// If no conditionType specified, return full SELECT
	if len(conditionType) == 0 {
		return BuildRawSelect(f, table)
	}

	// Build only requested parts, in the order provided
	parts := make([]string, 0, len(conditionType)*2)
	args := make([]any, 0)
	joinAdded := false
	for _, ct := range conditionType {
		norm := normalizeConditionType(ct)
		switch norm {
		case "SELECT":
			parts = append(parts, fmt.Sprintf("SELECT %s", cols))
		case "FROM":
			parts = append(parts, fmt.Sprintf("FROM %s", quoteIdent(table)))
		case "WHERE", "LIKE":
			if where != "" {
				parts = append(parts, "WHERE "+where)
				args = append(args, whereArgs...)
			}
		case "ORDER BY", "SORT":
			if orderBy != "" {
				parts = append(parts, orderBy)
			}
		case "LIMIT":
			if limitOffset != "" {
				if strings.HasPrefix(limitOffset, "LIMIT ") {
					parts = append(parts, limitOffset)
				}
			}
		case "OFFSET":
			if limitOffset != "" {
				if strings.HasPrefix(limitOffset, "OFFSET ") {
					parts = append(parts, limitOffset)
				} else if strings.Contains(limitOffset, " OFFSET ") {
					idx := strings.Index(limitOffset, " OFFSET ")
					parts = append(parts, limitOffset[idx+1:])
				}
			}
		case "PAGE":
			if limitOffset != "" {
				parts = append(parts, limitOffset)
			}
		case "JOIN":
			if joinSQL != "" && !joinAdded {
				parts = append(parts, joinSQL)
				args = append(args, joinArgs...)
				joinAdded = true
			}
		case "GROUP BY":
			// Not supported in core; tolerate silently
		}
	}

	fullSQL := strings.Join(parts, " ")
	return fullSQL, args
}

func normalizeConditionType(s string) string {
	up := strings.ToUpper(strings.TrimSpace(s))
	switch up {
	case "SORT":
		return "SORT"
	case "PAGE":
		return "PAGE"
	case "GROUPBY", "GROUP", "GORUP BY", "GORUPBY":
		return "GROUP BY"
	default:
		return up
	}
}

// buildJoins constructs INNER JOIN clauses for all preloads with ON conditions
// derived from each preload's expression. Since schema metadata is unavailable,
// the ON clause uses the preload's filters only (equivalent to ON 1=1 AND (...)).
func buildJoins(f Figo) (string, []any) {
	pre := f.GetPreloads()
	if len(pre) == 0 {
		return "", nil
	}
	tables := make([]string, 0, len(pre))
	for table := range pre {
		tables = append(tables, table)
	}
	sort.Strings(tables) // deterministic JOIN order

	parts := make([]string, 0, len(pre))
	args := make([]any, 0)
	for _, table := range tables {
		onSQL, onArgs := buildWhereFromExprsQualified(pre[table], table)
		if onSQL == "" {
			parts = append(parts, fmt.Sprintf("JOIN %s ON 1=1", quoteIdent(table)))
			continue
		}
		parts = append(parts, fmt.Sprintf("JOIN %s ON %s", quoteIdent(table), onSQL))
		args = append(args, onArgs...)
	}
	return strings.Join(parts, " "), args
}

func buildWhereFromExprsQualified(exprs []Expr, qualifier string) (string, []any) {
	if len(exprs) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(exprs))
	args := make([]any, 0)
	for _, e := range exprs {
		if e == nil {
			continue
		}
		p, a := exprToSQLQualified(e, qualifier)
		if p != "" {
			parts = append(parts, p)
			args = append(args, a...)
		}
	}
	return strings.Join(parts, " AND "), args
}

// expandPlaceholders replaces '?' with SQL literals derived from args in order.
// This is intended for debugging/logging, similar to GORM's DryRun Explain.
func expandPlaceholders(sql string, args []any) string {
	if len(args) == 0 {
		return sql
	}
	var b strings.Builder
	b.Grow(len(sql) + len(args)*4)

	idx := 0
	inSingle := false
	inDouble := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			b.WriteByte(ch)
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			b.WriteByte(ch)
			continue
		}
		if ch == '?' && !inSingle && !inDouble && idx < len(args) {
			b.WriteString(toSQLLiteral(args[idx]))
			idx++
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func toSQLLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int, int8, int16, int32, int64:
		return fmt.Sprintf("%v", x)
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%v", x)
	case float32, float64:
		return fmt.Sprintf("%v", x)
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case time.Time:
		// Render as a SQL datetime literal, not Go's "... +0000 UTC" String().
		return "'" + x.Format("2006-01-02 15:04:05") + "'"
	case string:
		return "'" + escapeSQLString(x) + "'"
	default:
		// Fallback: quote stringified value
		return "'" + escapeSQLString(fmt.Sprintf("%v", x)) + "'"
	}
}

// escapeSQLString escapes a value for embedding in a single-quoted SQL string
// literal. NOTE: this interpolated path (GetSqlString) is inherently riskier
// than parametrized queries — prefer GetQuery. We double single quotes per ANSI
// SQL and also escape backslashes because MySQL's default mode treats '\' as an
// escape character (a trailing '\' could otherwise escape the closing quote).
func escapeSQLString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "''")
	return s
}

func normalizeColumnName(f Figo, name string) string {
	switch f.GetNamingStrategy() {
	case NAMING_STRATEGY_SNAKE_CASE:
		result := stringy.New(name).SnakeCase("?", "").ToLower()
		// If stringy returns empty string, fallback to original name
		if result == "" {
			return name
		}
		return result
	case NAMING_STRATEGY_NO_CHANGE:
		fallthrough
	default:
		return name
	}
}
