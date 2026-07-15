package adapters

import (
	figo "github.com/bi0dread/figo/v4"

	"strings"
)

// SQLDialect describes how the raw adapter renders dialect-specific SQL:
// identifier quoting, bind-placeholder style, the regex operator, string
// literal escaping, and the "no limit" token for bare OFFSET.
//
// Select one on the adapter — the zero-value adapter keeps the historical
// MySQL rendering:
//
//	f.Build(figo.RawAdapter{})                            // MySQL (default)
//	f.Build(figo.RawAdapter{Dialect: figo.PostgresDialect}) // "col", $1, ~
//	f.Build(figo.RawAdapter{Dialect: figo.SQLiteDialect})   // "col", ?, REGEXP
//
// For a variant, copy a built-in and adjust it:
//
//	pg := *figo.PostgresDialect
//	pg.RegexOperator = "~*" // case-insensitive regex
//	f.Build(figo.RawAdapter{Dialect: &pg})
type SQLDialect struct {
	Name string

	// QuoteRune wraps identifiers (column/table names). Embedded quote runes
	// are escaped by doubling — identifiers can't be parametrized, so this is
	// the injection defense on both the GetSqlString and GetQuery paths.
	QuoteRune rune

	// NumberedPlaceholders renders binds as $1..$N (PostgreSQL) instead of ?.
	NumberedPlaceholders bool

	// RegexOperator renders figo.RegexExpr (=~ / !=~): REGEXP on MySQL/SQLite,
	// ~ (or ~* for case-insensitive) on PostgreSQL.
	RegexOperator string

	// EscapeBackslash doubles backslashes in interpolated string literals.
	// MySQL's default mode treats '\' as an escape character (a trailing '\'
	// could otherwise escape the closing quote); PostgreSQL (with standard
	// conforming strings) and SQLite treat it literally.
	EscapeBackslash bool

	// NoLimitToken is the LIMIT value paired with a bare OFFSET (OFFSET
	// without LIMIT is a syntax error on MySQL/SQLite).
	NoLimitToken string
}

// MySQLDialect is the default: backtick identifiers, ? placeholders, REGEXP.
var MySQLDialect = &SQLDialect{
	Name:                 "mysql",
	QuoteRune:            '`',
	NumberedPlaceholders: false,
	RegexOperator:        "REGEXP",
	EscapeBackslash:      true,
	NoLimitToken:         "18446744073709551615",
}

// PostgresDialect: double-quoted identifiers, $1..$N placeholders, ~ regex.
var PostgresDialect = &SQLDialect{
	Name:                 "postgres",
	QuoteRune:            '"',
	NumberedPlaceholders: true,
	RegexOperator:        "~",
	EscapeBackslash:      false,
	NoLimitToken:         "ALL",
}

// SQLiteDialect: double-quoted identifiers, ? placeholders, REGEXP (requires
// a registered REGEXP function on the connection).
var SQLiteDialect = &SQLDialect{
	Name:                 "sqlite",
	QuoteRune:            '"',
	NumberedPlaceholders: false,
	RegexOperator:        "REGEXP",
	EscapeBackslash:      false,
	NoLimitToken:         "-1",
}

// quoteIdent quotes an identifier with the dialect's quote rune, escaping
// embedded quote runes by doubling them.
func (d *SQLDialect) quoteIdent(ident string) string {
	q := string(d.QuoteRune)
	return q + strings.ReplaceAll(ident, q, q+q) + q
}

// escapeString escapes a value for embedding in a single-quoted SQL string
// literal. Single quotes are doubled per ANSI SQL; backslashes are doubled
// only on dialects that treat them as escapes.
func (d *SQLDialect) escapeString(s string) string {
	if d.EscapeBackslash {
		s = strings.ReplaceAll(s, "\\", "\\\\")
	}
	return strings.ReplaceAll(s, "'", "''")
}

// numberPlaceholders rewrites ?-style binds to $1..$N, skipping quoted
// regions (string literals and quoted identifiers) so a literal '?' inside
// them is never renumbered.
func numberPlaceholders(sql string) string {
	var b strings.Builder
	b.Grow(len(sql) + 8)

	n := 0
	inSingle := false
	inDouble := false
	inBacktick := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if ch == '\'' && !inDouble && !inBacktick {
			inSingle = !inSingle
		} else if ch == '"' && !inSingle && !inBacktick {
			inDouble = !inDouble
		} else if ch == '`' && !inSingle && !inDouble {
			inBacktick = !inBacktick
		} else if ch == '?' && !inSingle && !inDouble && !inBacktick {
			n++
			b.WriteByte('$')
			b.WriteString(itoa(n))
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

// itoa avoids strconv for the tiny positive ints placeholders need.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// rawDialectOf resolves the dialect for rendering: the RawAdapter's dialect
// when one is configured on the instance, MySQL otherwise (also the fallback
// for the package-level Build* helpers used without a raw adapter).
func rawDialectOf(f figo.Figo) *SQLDialect {
	if ra, ok := f.GetAdapterObject().(RawAdapter); ok {
		return ra.dialect()
	}
	return MySQLDialect
}
