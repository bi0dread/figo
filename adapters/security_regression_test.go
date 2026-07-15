package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// #1: a malformed/unclosed load bracket must not panic.
func TestUnclosedLoadDoesNotPanic(t *testing.T) {
	for _, in := range []string{`load=[`, `a=1 load=[`, `status="x" load=[`, `load=[T:id=1`} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("input %q panicked: %v", in, r)
				}
			}()
			f := New()
			_ = f.AddFiltersFromString(in)
			f.Build(RawAdapter{})
			_, _ = BuildRawWhere(f) // must not panic
		}()
	}
}

// #6: an empty IN set must match nothing (not drop the predicate → match all).
func TestEmptyInDoesNotBypassFilter(t *testing.T) {
	f := New()
	f.AddFiltersFromString(`id<in>[]`)
	f.Build(RawAdapter{})
	where, _ := BuildRawWhere(f)
	assert.Equal(t, "1=0", where, "empty IN must match nothing, not vanish")

	// Empty NOT IN matches everything.
	f2 := New()
	f2.AddFilter(NotInExpr{Field: "id", Values: nil})
	f2.Build(RawAdapter{})
	where2, _ := BuildRawWhere(f2)
	assert.Equal(t, "1=1", where2)

	// And it must still combine correctly inside AND.
	f3 := New()
	f3.AddFiltersFromString(`status="active" and id<in>[]`)
	f3.Build(RawAdapter{})
	where3, _ := BuildRawWhere(f3)
	assert.Contains(t, where3, "1=0")
	assert.Contains(t, where3, "status")
}

// #3: a quote rune in an identifier must be escaped (doubled), not break out
// of quoting. quoteIdent is the single defense for both the GetSqlString and
// GetQuery paths (identifiers can't be parametrized).
func TestIdentifierBacktickIsEscaped(t *testing.T) {
	assert.Equal(t, "`id``x`", MySQLDialect.quoteIdent("id`x"))
	assert.Equal(t, "`id`` = 1 OR 1=1 -- `", MySQLDialect.quoteIdent("id` = 1 OR 1=1 -- "))
	// A plain identifier is unchanged apart from the delimiters.
	assert.Equal(t, "`user_id`", MySQLDialect.quoteIdent("user_id"))

	// Postgres identifiers escape embedded double quotes the same way.
	assert.Equal(t, `"id""x"`, PostgresDialect.quoteIdent(`id"x`))
	assert.Equal(t, `"user_id"`, PostgresDialect.quoteIdent("user_id"))
}

// #2: single quotes in a string value must be escaped in the interpolated SQL.
func TestStringLiteralEscapesSingleQuote(t *testing.T) {
	got := toSQLLiteral(MySQLDialect, "O'Brien")
	assert.Equal(t, "'O''Brien'", got)

	// A classic injection payload stays inside the literal.
	inj := toSQLLiteral(MySQLDialect, "x' OR '1'='1")
	assert.Equal(t, "'x'' OR ''1''=''1'", inj)
	// Every single quote in the output is doubled — no lone quote can terminate
	// the literal early.
	assert.Equal(t, 0, countLoneSingleQuotes(inj))

	// Backslash is neutralized (MySQL default mode).
	assert.Equal(t, `'a\\b'`, toSQLLiteral(MySQLDialect, `a\b`))

	// Postgres/SQLite treat backslash literally — no doubling, quotes still doubled.
	assert.Equal(t, `'a\b'`, toSQLLiteral(PostgresDialect, `a\b`))
	assert.Equal(t, "'O''Brien'", toSQLLiteral(PostgresDialect, "O'Brien"))
}

// countLoneSingleQuotes returns how many single quotes are NOT part of a doubled
// pair, ignoring the two delimiter quotes at the very ends.
func countLoneSingleQuotes(s string) int {
	inner := strings.TrimPrefix(strings.TrimSuffix(s, "'"), "'")
	n := 0
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\'' {
			if i+1 < len(inner) && inner[i+1] == '\'' {
				i++ // skip the pair
				continue
			}
			n++
		}
	}
	return n
}
