package adapters

import (
	figo "github.com/bi0dread/figo/v4"

	"fmt"
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

// RawPreload represents a built WHERE clause and args for a preload relationship
type RawPreload struct {
	Where string
	Args  []any
}

// BuildRawPreloads builds WHERE clauses for each preload relationship without any ORM dependency
func BuildRawPreloads(f figo.Figo) map[string]RawPreload {
	d := rawDialectOf(f)
	result := make(map[string]RawPreload)
	for rel, exprs := range f.GetPreloads() {
		where, args := buildWhereFromExprs(d, exprs)
		if d.NumberedPlaceholders {
			where = numberPlaceholders(where)
		}
		result[rel] = RawPreload{Where: where, Args: args}
	}
	return result
}

// BuildRawWhere builds a SQL WHERE clause (without the leading WHERE keyword) and its args
func BuildRawWhere(f figo.Figo) (string, []any) {
	d := rawDialectOf(f)
	where, args := buildWhereFromExprs(d, f.GetClauses())
	if d.NumberedPlaceholders {
		where = numberPlaceholders(where)
	}
	return where, args
}

// BuildRawSelect builds a full SELECT query for the given table and columns.
// Identifier quoting and placeholder style come from the instance's raw
// adapter dialect (MySQL backticks and '?' by default).
func BuildRawSelect(f figo.Figo, table string, columns ...string) (string, []any) {
	d := rawDialectOf(f)
	sql, args := buildFullSelect(d, f, table, columns...)
	if d.NumberedPlaceholders {
		sql = numberPlaceholders(sql)
	}
	return sql, args
}

// -- internals --
// Internal builders always emit '?' placeholders; numbered dialects rewrite
// them ONCE on the fully assembled statement (numbering fragments and then
// concatenating them would repeat $1).

func buildWhereFromExprs(d *SQLDialect, exprs []figo.Expr) (string, []any) {
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
		p, a := exprToSQL(d, e)
		if p != "" {
			parts = append(parts, p)
			args = append(args, a...)
		}
	}
	return strings.Join(parts, " AND "), args
}

func exprToSQL(d *SQLDialect, e figo.Expr) (string, []any) {
	switch x := e.(type) {
	case figo.EqExpr:
		return fmt.Sprintf("%s = ?", d.quoteIdent(x.Field)), []any{x.Value}
	case figo.GteExpr:
		return fmt.Sprintf("%s >= ?", d.quoteIdent(x.Field)), []any{x.Value}
	case figo.GtExpr:
		return fmt.Sprintf("%s > ?", d.quoteIdent(x.Field)), []any{x.Value}
	case figo.LtExpr:
		return fmt.Sprintf("%s < ?", d.quoteIdent(x.Field)), []any{x.Value}
	case figo.LteExpr:
		return fmt.Sprintf("%s <= ?", d.quoteIdent(x.Field)), []any{x.Value}
	case figo.NeqExpr:
		return fmt.Sprintf("%s != ?", d.quoteIdent(x.Field)), []any{x.Value}
	case figo.LikeExpr:
		return fmt.Sprintf("%s LIKE ?", d.quoteIdent(x.Field)), []any{x.Value}
	case figo.RegexExpr:
		// The operator comes from the dialect: REGEXP (MySQL/SQLite), ~ (Postgres).
		return fmt.Sprintf("%s %s ?", d.quoteIdent(x.Field), d.RegexOperator), []any{x.Value}
	case figo.ILikeExpr:
		return fmt.Sprintf("LOWER(%s) LIKE LOWER(?)", d.quoteIdent(x.Field)), []any{x.Value}
	case figo.IsNullExpr:
		return fmt.Sprintf("%s IS NULL", d.quoteIdent(x.Field)), nil
	case figo.NotNullExpr:
		return fmt.Sprintf("%s IS NOT NULL", d.quoteIdent(x.Field)), nil
	case figo.InExpr:
		if len(x.Values) == 0 {
			// An empty IN set matches nothing. Returning "" would drop the whole
			// predicate (WHERE disappears), turning a match-nothing filter into a
			// match-everything one — a filter/authorization bypass.
			return "1=0", nil
		}
		placeholders := strings.Repeat("?,", len(x.Values))
		placeholders = placeholders[:len(placeholders)-1]
		return fmt.Sprintf("%s IN (%s)", d.quoteIdent(x.Field), placeholders), append([]any{}, x.Values...)
	case figo.NotInExpr:
		if len(x.Values) == 0 {
			// "NOT IN (empty set)" is true for every row.
			return "1=1", nil
		}
		placeholders := strings.Repeat("?,", len(x.Values))
		placeholders = placeholders[:len(placeholders)-1]
		return fmt.Sprintf("%s NOT IN (%s)", d.quoteIdent(x.Field), placeholders), append([]any{}, x.Values...)
	case figo.BetweenExpr:
		return fmt.Sprintf("%s BETWEEN ? AND ?", d.quoteIdent(x.Field)), []any{x.Low, x.High}
	case figo.AndExpr:
		return joinGroup(d, "AND", x.Operands)
	case figo.OrExpr:
		return joinGroup(d, "OR", x.Operands)
	case figo.NotExpr:
		if len(x.Operands) == 0 {
			return "", nil
		}
		// figo.NotExpr means "none of the operands match": NOT(a OR b), matching
		// Mongo's $nor and GORM's clause.Not. Joining with AND here rendered
		// NOT(a AND b), which matches rows the other adapters exclude.
		inner, args := joinGroup(d, "OR", x.Operands)
		if inner == "" {
			return "", nil
		}
		return fmt.Sprintf("NOT (%s)", inner), args
	case figo.OrderBy:
		// handled separately in buildOrderBy
		return "", nil
	default:
		return "", nil
	}
}

func exprToSQLQualified(d *SQLDialect, e figo.Expr, qualifier string) (string, []any) {
	// Qualify column references with the given table name
	qcol := func(field string) string { return d.quoteIdent(qualifier) + "." + d.quoteIdent(field) }
	switch x := e.(type) {
	case figo.EqExpr:
		return fmt.Sprintf("%s = ?", qcol(x.Field)), []any{x.Value}
	case figo.GteExpr:
		return fmt.Sprintf("%s >= ?", qcol(x.Field)), []any{x.Value}
	case figo.GtExpr:
		return fmt.Sprintf("%s > ?", qcol(x.Field)), []any{x.Value}
	case figo.LtExpr:
		return fmt.Sprintf("%s < ?", qcol(x.Field)), []any{x.Value}
	case figo.LteExpr:
		return fmt.Sprintf("%s <= ?", qcol(x.Field)), []any{x.Value}
	case figo.NeqExpr:
		return fmt.Sprintf("%s != ?", qcol(x.Field)), []any{x.Value}
	case figo.LikeExpr:
		return fmt.Sprintf("%s LIKE ?", qcol(x.Field)), []any{x.Value}
	case figo.RegexExpr:
		return fmt.Sprintf("%s %s ?", qcol(x.Field), d.RegexOperator), []any{x.Value}
	case figo.ILikeExpr:
		return fmt.Sprintf("LOWER(%s) LIKE LOWER(?)", qcol(x.Field)), []any{x.Value}
	case figo.IsNullExpr:
		return fmt.Sprintf("%s IS NULL", qcol(x.Field)), nil
	case figo.NotNullExpr:
		return fmt.Sprintf("%s IS NOT NULL", qcol(x.Field)), nil
	case figo.InExpr:
		if len(x.Values) == 0 {
			// Empty IN set matches nothing; see exprToSQL for rationale.
			return "1=0", nil
		}
		placeholders := strings.Repeat("?,", len(x.Values))
		placeholders = placeholders[:len(placeholders)-1]
		return fmt.Sprintf("%s IN (%s)", qcol(x.Field), placeholders), append([]any{}, x.Values...)
	case figo.NotInExpr:
		if len(x.Values) == 0 {
			// "NOT IN (empty set)" is true for every row.
			return "1=1", nil
		}
		placeholders := strings.Repeat("?,", len(x.Values))
		placeholders = placeholders[:len(placeholders)-1]
		return fmt.Sprintf("%s NOT IN (%s)", qcol(x.Field), placeholders), append([]any{}, x.Values...)
	case figo.BetweenExpr:
		return fmt.Sprintf("%s BETWEEN ? AND ?", qcol(x.Field)), []any{x.Low, x.High}
	case figo.AndExpr:
		return joinGroupQualified(d, "AND", x.Operands, qualifier)
	case figo.OrExpr:
		return joinGroupQualified(d, "OR", x.Operands, qualifier)
	case figo.NotExpr:
		if len(x.Operands) == 0 {
			return "", nil
		}
		// See exprToSQL: figo.NotExpr is NOT(a OR b) across all adapters.
		inner, args := joinGroupQualified(d, "OR", x.Operands, qualifier)
		if inner == "" {
			return "", nil
		}
		return fmt.Sprintf("NOT (%s)", inner), args
	case figo.OrderBy:
		return "", nil
	default:
		return "", nil
	}
}

func joinGroup(d *SQLDialect, op string, operands []figo.Expr) (string, []any) {
	parts := make([]string, 0, len(operands))
	args := make([]any, 0)
	for _, e := range operands {
		if e == nil {
			continue
		}
		p, a := exprToSQL(d, e)
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

func joinGroupQualified(d *SQLDialect, op string, operands []figo.Expr, qualifier string) (string, []any) {
	parts := make([]string, 0, len(operands))
	args := make([]any, 0)
	for _, e := range operands {
		if e == nil {
			continue
		}
		p, a := exprToSQLQualified(d, e, qualifier)
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

func buildOrderBy(d *SQLDialect, f figo.Figo) string {
	sort := f.GetSort()
	if sort != nil {
		cols := make([]string, 0, len(sort.Columns))
		for _, c := range sort.Columns {
			dir := "ASC"
			if c.Desc {
				dir = "DESC"
			}
			cols = append(cols, fmt.Sprintf("%s %s", d.quoteIdent(c.Name), dir))
		}
		if len(cols) > 0 {
			return "ORDER BY " + strings.Join(cols, ", ")
		}
	}
	return ""
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
// (RawAdapter.GetQuery numbers them for Postgres).
func AdapterRawGetSql(f figo.Figo, ctx any, conditionType ...string) (string, []any, bool) {
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

func (a RawAdapter) GetSqlString(f figo.Figo, ctx any, conditionType ...string) (string, bool) {
	if f == nil {
		return "", false
	}
	sql, args, ok := AdapterRawGetSql(f, ctx, conditionType...)
	if !ok {
		return "", false
	}
	// Interpolate literals into the ?-form; numbering never applies here.
	return expandPlaceholders(a.dialect(), sql, args), true
}

func (a RawAdapter) GetQuery(f figo.Figo, ctx any, conditionType ...string) (figo.Query, bool) {
	if f == nil {
		return nil, false
	}
	sql, args, ok := AdapterRawGetSql(f, ctx, conditionType...)
	if !ok {
		return nil, false
	}
	if a.dialect().NumberedPlaceholders {
		sql = numberPlaceholders(sql)
	}
	return figo.SQLQuery{SQL: sql, Args: args}, true
}

func buildByConditions(f figo.Figo, table string, conditionType ...string) (string, []any) {
	d := rawDialectOf(f)

	// Determine columns from selectFields; default to *
	cols := "*"
	if sel := f.GetSelectFields(); len(sel) > 0 {
		quoted := make([]string, 0, len(sel))
		for _, name := range sortedKeys(sel) {
			quoted = append(quoted, d.quoteIdent(normalizeColumnName(f, name)))
		}
		cols = strings.Join(quoted, ", ")
	}

	joinSQL, joinArgs := buildJoins(d, f)
	where, whereArgs := buildWhereFromExprs(d, f.GetClauses())
	orderBy := buildOrderBy(d, f)
	limitOffset := buildLimitOffset(d, f)

	// If no conditionType specified, return full SELECT (?-form; the adapter
	// numbers or interpolates at its boundary)
	if len(conditionType) == 0 {
		return buildFullSelect(d, f, table)
	}

	// Build only requested parts, in the order provided
	parts := make([]string, 0, len(conditionType)*2)
	args := make([]any, 0)
	joinAdded := false
	whereAdded := false
	orderAdded := false
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
			// OFFSET, and "LIMIT","OFFSET" must not duplicate it.
			if strings.HasPrefix(limitOffset, "LIMIT ") {
				if idx := strings.Index(limitOffset, " OFFSET "); idx >= 0 {
					parts = append(parts, limitOffset[:idx])
				} else {
					parts = append(parts, limitOffset)
				}
			}
		case "OFFSET":
			if idx := strings.Index(limitOffset, "OFFSET "); idx >= 0 {
				parts = append(parts, limitOffset[idx:])
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

// buildFullSelect assembles the complete SELECT in ?-form (numbering, when the
// dialect requires it, happens at the adapter/helper boundary). Explicit
// columns are used only when the instance has no select fields.
func buildFullSelect(d *SQLDialect, f figo.Figo, table string, columns ...string) (string, []any) {
	cols := columnsOnly(f, d)
	if cols == "*" && len(columns) > 0 {
		quoted := make([]string, 0, len(columns))
		for _, c := range columns {
			quoted = append(quoted, d.quoteIdent(c))
		}
		cols = strings.Join(quoted, ", ")
	}

	joinSQL, joinArgs := buildJoins(d, f)
	where, whereArgs := buildWhereFromExprs(d, f.GetClauses())
	orderBy := buildOrderBy(d, f)
	limitOffset := buildLimitOffset(d, f)

	query := fmt.Sprintf("SELECT %s FROM %s", cols, d.quoteIdent(table))
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
	args := append([]any{}, joinArgs...)
	args = append(args, whereArgs...)
	return query, args
}

// columnsOnly renders the SELECT column list from the instance's selects.
func columnsOnly(f figo.Figo, d *SQLDialect) string {
	if sel := f.GetSelectFields(); len(sel) > 0 {
		quoted := make([]string, 0, len(sel))
		for _, name := range sortedKeys(sel) {
			quoted = append(quoted, d.quoteIdent(normalizeColumnName(f, name)))
		}
		return strings.Join(quoted, ", ")
	}
	return "*"
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
func buildJoins(d *SQLDialect, f figo.Figo) (string, []any) {
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
		onSQL, onArgs := buildWhereFromExprsQualified(d, pre[table], table)
		if onSQL == "" {
			parts = append(parts, fmt.Sprintf("JOIN %s ON 1=1", d.quoteIdent(table)))
			continue
		}
		parts = append(parts, fmt.Sprintf("JOIN %s ON %s", d.quoteIdent(table), onSQL))
		args = append(args, onArgs...)
	}
	return strings.Join(parts, " "), args
}

func buildWhereFromExprsQualified(d *SQLDialect, exprs []figo.Expr, qualifier string) (string, []any) {
	if len(exprs) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(exprs))
	args := make([]any, 0)
	for _, e := range exprs {
		if e == nil {
			continue
		}
		p, a := exprToSQLQualified(d, e, qualifier)
		if p != "" {
			parts = append(parts, p)
			args = append(args, a...)
		}
	}
	return strings.Join(parts, " AND "), args
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
		return "'" + d.escapeString(x) + "'"
	default:
		// Fallback: quote stringified value
		return "'" + d.escapeString(fmt.Sprintf("%v", x)) + "'"
	}
}

func normalizeColumnName(f figo.Figo, name string) string {
	// Apply the instance's naming func — the same conversion the parser runs,
	// so a deliberately preserved camelCase name is not re-snake_cased here.
	return f.GetNamingFunc()(name) // never nil: SnakeCaseNaming is the default
}
