package figo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// whereFor renders the raw SQL WHERE clause (and args) for a DSL string.
func whereFor(t *testing.T, dsl string) (string, []any) {
	t.Helper()
	f := New(RawAdapter{})
	if err := f.AddFiltersFromString(dsl); err != nil {
		t.Fatalf("AddFiltersFromString(%q): %v", dsl, err)
	}
	f.Build()
	return BuildRawWhere(f)
}

// TestNotIsNotDropped guards against a regression where a leading NOT applied to
// a single operand or a parenthesized group was silently dropped by
// processWithPrecedence's single-expression short-circuit, inverting query semantics.
func TestNotIsNotDropped(t *testing.T) {
	t.Run("SingleOperand", func(t *testing.T) {
		where, args := whereFor(t, `status="deleted"`)
		assert.NotContains(t, where, "NOT")
		notWhere, notArgs := whereFor(t, `not status="deleted"`)
		assert.Contains(t, notWhere, "NOT", "NOT must not be dropped for a single operand")
		assert.Equal(t, args, notArgs)
	})

	t.Run("ParenthesizedGroup", func(t *testing.T) {
		where, _ := whereFor(t, `not (a=1 or b=2)`)
		assert.Contains(t, where, "NOT", "NOT must not be dropped for a parenthesized group")
		assert.Contains(t, where, "OR")
	})
}

// TestParenthesesGroupingAndValues guards against a tokenizer regression where a
// closing ')' abutting a value (no preceding space) was swallowed into the value
// ("2)" instead of "2") and the group was never closed, corrupting both the
// argument and the operator grouping.
func TestParenthesesGroupingAndValues(t *testing.T) {
	t.Run("NoSpaceBeforeParen", func(t *testing.T) {
		where, args := whereFor(t, `(a=1 or b=2) and c=3`)
		// Grouping: (a OR b) AND c, not a OR (b AND c)
		assert.Equal(t, "((`a` = ? OR `b` = ?) AND `c` = ?)", where)
		// Args must be clean integers, not the corrupted "2)".
		assert.Equal(t, []any{int64(1), int64(2), int64(3)}, args)
	})

	t.Run("ParenAbuttingKeyword", func(t *testing.T) {
		where, args := whereFor(t, `(a=1 or b=2)and c=3`)
		assert.Equal(t, "((`a` = ? OR `b` = ?) AND `c` = ?)", where)
		assert.Equal(t, []any{int64(1), int64(2), int64(3)}, args)
	})

	t.Run("TrailingGroup", func(t *testing.T) {
		// The closing ')' is the final character of the input.
		where, args := whereFor(t, `id=1 and (age>20 or active=true)`)
		assert.Equal(t, "(`id` = ? AND (`age` > ? OR `active` = ?))", where)
		assert.Equal(t, []any{int64(1), int64(20), true}, args)
	})
}

// TestBetweenParensStillWork ensures the parenthesis-aware tokenizer does not
// break BETWEEN's overloaded "<bet>(low..high)" value syntax, including when the
// BETWEEN sits inside a logical group.
func TestBetweenParensStillWork(t *testing.T) {
	t.Run("Standalone", func(t *testing.T) {
		where, args := whereFor(t, `price<bet>(10..20)`)
		assert.Contains(t, where, "BETWEEN")
		assert.Equal(t, []any{int64(10), int64(20)}, args)
	})

	t.Run("InsideGroup", func(t *testing.T) {
		where, args := whereFor(t, `(price<bet>(10..20) or a=1) and b=2`)
		assert.Contains(t, where, "BETWEEN")
		assert.Equal(t, []any{int64(10), int64(20), int64(1), int64(2)}, args)
	})
}
