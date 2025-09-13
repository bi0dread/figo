package figo

import (
	"testing"
)

// TestBugFixes tests the critical bugs we found and fixed
func TestBugFixes(t *testing.T) {
	t.Run("StackUnderflowBug", func(t *testing.T) {
		// Test unmatched closing parentheses
		f := New(RawAdapter{})
		f.AddFiltersFromString(`(name = "test" and age > 25))`) // Extra closing parenthesis
		f.Build()
		// Should not panic
		sql, _ := BuildRawSelect(f, "test")
		if sql == "" {
			t.Error("Expected SQL to be generated")
		}
	})

	t.Run("MalformedLoadExpression", func(t *testing.T) {
		// Test malformed load expressions
		f := New(RawAdapter{})
		f.AddFiltersFromString(`load=[User:name="test" | Profile:bio="dev"`) // Missing closing bracket
		f.Build()
		// Should not panic
		sql, _ := BuildRawSelect(f, "test")
		if sql == "" {
			t.Error("Expected SQL to be generated")
		}
	})

	t.Run("EmptyLoadExpression", func(t *testing.T) {
		// Test empty load expressions
		f := New(RawAdapter{})
		f.AddFiltersFromString(`load=[]`) // Empty load
		f.Build()
		// Should not panic
		sql, _ := BuildRawSelect(f, "test")
		if sql == "" {
			t.Error("Expected SQL to be generated")
		}
	})

	t.Run("MalformedLoadContent", func(t *testing.T) {
		// Test malformed load content without colon
		f := New(RawAdapter{})
		f.AddFiltersFromString(`load=[User name="test" | Profile bio="dev"]`) // Missing colons
		f.Build()
		// Should not panic
		sql, _ := BuildRawSelect(f, "test")
		if sql == "" {
			t.Error("Expected SQL to be generated")
		}
	})

	t.Run("ComplexMalformedExpression", func(t *testing.T) {
		// Test complex malformed expressions
		f := New(RawAdapter{})
		f.AddFiltersFromString(`((name = "test" and age > 25) or (status = "active" and score > 80)) and (deleted_at <null> or updated_at > "2023-01-01") and load=[User:name="test" | Profile:bio="dev"`) // Missing closing bracket
		f.Build()
		// Should not panic
		sql, _ := BuildRawSelect(f, "test")
		if sql == "" {
			t.Error("Expected SQL to be generated")
		}
	})

	t.Run("EmptyExpression", func(t *testing.T) {
		// Test empty expressions
		f := New(RawAdapter{})
		f.AddFiltersFromString(``) // Empty string
		f.Build()
		// Should not panic
		sql, _ := BuildRawSelect(f, "test")
		if sql == "" {
			t.Error("Expected SQL to be generated")
		}
	})

	t.Run("OnlyWhitespace", func(t *testing.T) {
		// Test only whitespace
		f := New(RawAdapter{})
		f.AddFiltersFromString(`   `) // Only whitespace
		f.Build()
		// Should not panic
		sql, _ := BuildRawSelect(f, "test")
		if sql == "" {
			t.Error("Expected SQL to be generated")
		}
	})

	t.Run("OnlyParentheses", func(t *testing.T) {
		// Test only parentheses
		f := New(RawAdapter{})
		f.AddFiltersFromString(`((()))`) // Only parentheses
		f.Build()
		// Should not panic
		sql, _ := BuildRawSelect(f, "test")
		if sql == "" {
			t.Error("Expected SQL to be generated")
		}
	})

	t.Run("MalformedOperators", func(t *testing.T) {
		// Test malformed operators
		f := New(RawAdapter{})
		f.AddFiltersFromString(`name = and age > 25`) // Missing value after =
		f.Build()
		// Should not panic
		sql, _ := BuildRawSelect(f, "test")
		if sql == "" {
			t.Error("Expected SQL to be generated")
		}
	})

	t.Run("UnmatchedQuotes", func(t *testing.T) {
		// Test unmatched quotes
		f := New(RawAdapter{})
		f.AddFiltersFromString(`name = "test and age > 25`) // Missing closing quote
		f.Build()
		// Should not panic
		sql, _ := BuildRawSelect(f, "test")
		if sql == "" {
			t.Error("Expected SQL to be generated")
		}
	})
}
