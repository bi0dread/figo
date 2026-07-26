package adapters

import (
	figo "github.com/bi0dread/figo/v4"

	"fmt"
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
// embedded quote runes by doubling them. A dotted name is quoted per segment
// (users.first_name -> `users`.`first_name`) so a qualified reference set via
// Walk/SetNodeField renders as table.column instead of one quoted identifier
// naming a nonexistent dotted column. Every segment still has embedded quote
// runes doubled, so the injection hardening is unchanged.
func (d *SQLDialect) quoteIdent(ident string) string {
	q := string(d.QuoteRune)
	if strings.Contains(ident, ".") {
		segs := strings.Split(ident, ".")
		for i, s := range segs {
			segs[i] = q + strings.ReplaceAll(s, q, q+q) + q
		}
		return strings.Join(segs, ".")
	}
	return q + strings.ReplaceAll(ident, q, q+q) + q
}

// validateIdent rejects identifiers that quoting cannot turn into executable
// SQL. quoteIdent doubles the quote rune — that is the injection defense and it
// holds — but two classes of input survive it and still produce a syntax error
// at the engine, with BuildE nil and ok=true so the caller cannot tell:
//
//   - C0 control bytes. A NUL in particular is the only byte of 0x00-0x7f that
//     makes the rendered statement unparseable (`a<NUL>b` -> SQLite
//     "unrecognized token"), and drivers that pass identifiers to the engine as
//     C strings may instead TRUNCATE the name and read a different column.
//   - an empty name or an empty dot segment ("", "...", reached from DSL as
//     `...<0` / `sort=...:asc`), which render as zero-length delimited
//     identifiers -- a bare quoted empty string, or one per dot segment --
//     turns into a constant column.
//
// Failing closed here matches the rest of the adapter: an unrenderable input is
// an error, never SQL that means something else.
func validateIdent(kind, name string) error {
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("raw adapter: %s %q contains control byte 0x%02x", kind, name, c)
		}
	}
	for _, seg := range strings.Split(name, ".") {
		if seg == "" {
			return fmt.Errorf("raw adapter: %s %q has an empty name segment", kind, name)
		}
	}
	return nil
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
// them is never renumbered. On dialects where '\' escapes inside a string
// literal (EscapeBackslash), a backslash consumes the next byte: a CustomExpr
// fragment containing \' otherwise flipped the scanner out of the literal and
// every following placeholder was mis-numbered.
func numberPlaceholders(d *SQLDialect, sql string) string {
	var b strings.Builder
	b.Grow(len(sql) + 8)

	n := 0
	inSingle := false
	inDouble := false
	inBacktick := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if ch == '\\' && inSingle && d.EscapeBackslash && i+1 < len(sql) {
			b.WriteByte(ch)
			i++
			b.WriteByte(sql[i])
			continue
		}
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

// rawDialectOf resolves the dialect from the INSTANCE's adapter. It is for the
// receiver-less package-level Build* helpers only — the RawAdapter methods
// thread their own receiver's dialect instead, because an instance built with
// nil (or with a different adapter, or via SetAdapterObject after the fact)
// carries no RawAdapter to read, and the resulting mix of MySQL quoting with
// the receiver's $n placeholders is valid on no engine.
//
// Both the value and the pointer form are accepted: *RawAdapter satisfies
// figo.Adapter through the value receivers, so asserting only the value type
// silently fell back to MySQL for a caller who wrote Build(&RawAdapter{...}).
func rawDialectOf(f figo.Figo) *SQLDialect {
	switch ra := f.GetAdapterObject().(type) {
	case RawAdapter:
		return ra.dialect()
	case *RawAdapter:
		if ra != nil {
			return ra.dialect()
		}
	}
	return MySQLDialect
}
