package figo

import (
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
			f := New(RawAdapter{})
			_ = f.AddFiltersFromString(in)
			f.Build()
			_, _ = BuildRawWhere(f) // must not panic
		}()
	}
}

// #6: an empty IN set must match nothing (not drop the predicate → match all).
func TestEmptyInDoesNotBypassFilter(t *testing.T) {
	f := New(RawAdapter{})
	f.AddFiltersFromString(`id<in>[]`)
	f.Build()
	where, _ := BuildRawWhere(f)
	assert.Equal(t, "1=0", where, "empty IN must match nothing, not vanish")

	// Empty NOT IN matches everything.
	f2 := New(RawAdapter{})
	f2.AddFilter(NotInExpr{Field: "id", Values: nil})
	f2.Build()
	where2, _ := BuildRawWhere(f2)
	assert.Equal(t, "1=1", where2)

	// And it must still combine correctly inside AND.
	f3 := New(RawAdapter{})
	f3.AddFiltersFromString(`status="active" and id<in>[]`)
	f3.Build()
	where3, _ := BuildRawWhere(f3)
	assert.Contains(t, where3, "1=0")
	assert.Contains(t, where3, "status")
}

// #3: a backtick in an identifier must be escaped (doubled), not break out of
// quoting. quoteIdent is the single defense for both the GetSqlString and
// GetQuery paths (identifiers can't be parametrized).
func TestIdentifierBacktickIsEscaped(t *testing.T) {
	assert.Equal(t, "`id``x`", quoteIdent("id`x"))
	assert.Equal(t, "`id`` = 1 OR 1=1 -- `", quoteIdent("id` = 1 OR 1=1 -- "))
	// A plain identifier is unchanged apart from the delimiters.
	assert.Equal(t, "`user_id`", quoteIdent("user_id"))
}

// #2: single quotes in a string value must be escaped in the interpolated SQL.
func TestStringLiteralEscapesSingleQuote(t *testing.T) {
	got := toSQLLiteral("O'Brien")
	assert.Equal(t, "'O''Brien'", got)

	// A classic injection payload stays inside the literal.
	inj := toSQLLiteral("x' OR '1'='1")
	assert.Equal(t, "'x'' OR ''1''=''1'", inj)
	// Every single quote in the output is doubled — no lone quote can terminate
	// the literal early.
	assert.Equal(t, 0, countLoneSingleQuotes(inj))

	// Backslash is neutralized (MySQL default mode).
	assert.Equal(t, `'a\\b'`, toSQLLiteral(`a\b`))
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
