package figo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFieldWhitelisting(t *testing.T) {
	t.Run("FieldWhitelistDisabled", func(t *testing.T) {
		f := New(RawAdapter{})
		f.DisableFieldWhitelist()

		// All fields should be allowed when whitelist is disabled
		assert.True(t, f.IsFieldAllowed("any_field"))
		assert.True(t, f.IsFieldAllowed("sensitive_data"))
	})

	t.Run("FieldWhitelistEnabled", func(t *testing.T) {
		f := New(RawAdapter{})
		f.SetAllowedFields("id", "name", "email", "created_at")
		f.EnableFieldWhitelist()

		// Allowed fields should pass
		assert.True(t, f.IsFieldAllowed("id"))
		assert.True(t, f.IsFieldAllowed("name"))
		assert.True(t, f.IsFieldAllowed("email"))
		assert.True(t, f.IsFieldAllowed("created_at"))

		// Disallowed fields should be blocked
		assert.False(t, f.IsFieldAllowed("password"))
		assert.False(t, f.IsFieldAllowed("secret_key"))
		assert.False(t, f.IsFieldAllowed("internal_data"))
	})

	t.Run("FieldWhitelistWithDSL", func(t *testing.T) {
		f := New(RawAdapter{})
		f.SetAllowedFields("id", "name", "email")
		f.EnableFieldWhitelist()

		// Add filters with allowed and disallowed fields
		f.AddFiltersFromString(`id=1 and name="test" and password="secret"`)
		f.Build()

		// Only allowed fields should be processed
		clauses := f.GetClauses()
		assert.Len(t, clauses, 1) // Should be a single AndExpr clause

		// Verify the clause contains only allowed fields
		fieldNames := make(map[string]bool)
		andExpr, ok := clauses[0].(AndExpr)
		assert.True(t, ok, "Expected AndExpr clause")

		for _, operand := range andExpr.Operands {
			switch c := operand.(type) {
			case EqExpr:
				fieldNames[c.Field] = true
			}
		}

		assert.True(t, fieldNames["id"])
		assert.True(t, fieldNames["name"])
		assert.False(t, fieldNames["password"])
	})
}

func TestQueryLimits(t *testing.T) {
	t.Run("DefaultLimits", func(t *testing.T) {
		f := New(RawAdapter{})
		limits := f.GetQueryLimits()

		assert.Equal(t, 10, limits.MaxNestingDepth)
		assert.Equal(t, 50, limits.MaxFieldCount)
		assert.Equal(t, 100, limits.MaxParameterCount)
		assert.Equal(t, 200, limits.MaxExpressionCount)
	})

	t.Run("CustomLimits", func(t *testing.T) {
		f := New(RawAdapter{})
		customLimits := QueryLimits{
			MaxNestingDepth:    5,
			MaxFieldCount:      10,
			MaxParameterCount:  20,
			MaxExpressionCount: 50,
		}
		f.SetQueryLimits(customLimits)

		limits := f.GetQueryLimits()
		assert.Equal(t, 5, limits.MaxNestingDepth)
		assert.Equal(t, 10, limits.MaxFieldCount)
		assert.Equal(t, 20, limits.MaxParameterCount)
		assert.Equal(t, 50, limits.MaxExpressionCount)
	})
}

func TestEnhancedTypeParsing(t *testing.T) {
	t.Run("DateParsing", func(t *testing.T) {
		f := New(RawAdapter{})

		// Test various date formats
		testCases := []struct {
			input    string
			expected time.Time
		}{
			{`"2023-01-15T10:30:00Z"`, time.Date(2023, 1, 15, 10, 30, 0, 0, time.UTC)},
			{`"2023-01-15"`, time.Date(2023, 1, 15, 0, 0, 0, 0, time.UTC)},
			{`"2023/01/15"`, time.Date(2023, 1, 15, 0, 0, 0, 0, time.UTC)},
			{`"Jan 15, 2023"`, time.Date(2023, 1, 15, 0, 0, 0, 0, time.UTC)},
		}

		for _, tc := range testCases {
			t.Run(tc.input, func(t *testing.T) {
				// Parse the value
				value := f.ParseFieldsValue(tc.input)

				// Should be a time.Time
				if dateVal, ok := value.(time.Time); ok {
					assert.Equal(t, tc.expected.Year(), dateVal.Year())
					assert.Equal(t, tc.expected.Month(), dateVal.Month())
					assert.Equal(t, tc.expected.Day(), dateVal.Day())
				} else {
					t.Errorf("Expected time.Time, got %T", value)
				}
			})
		}
	})

	t.Run("NullParsing", func(t *testing.T) {
		f := New(RawAdapter{})

		// Test null values
		assert.Nil(t, f.ParseFieldsValue("null"))
		assert.Nil(t, f.ParseFieldsValue("NULL"))
	})

	t.Run("BooleanParsing", func(t *testing.T) {
		f := New(RawAdapter{})

		// Test boolean values
		assert.True(t, f.ParseFieldsValue("true").(bool))
		assert.False(t, f.ParseFieldsValue("false").(bool))
	})

	t.Run("NumericParsing", func(t *testing.T) {
		f := New(RawAdapter{})

		// Test numeric values
		assert.Equal(t, int64(123), f.ParseFieldsValue("123"))
		assert.Equal(t, float64(123.45), f.ParseFieldsValue("123.45"))
	})

	t.Run("StringParsing", func(t *testing.T) {
		f := New(RawAdapter{})

		// Test string values
		assert.Equal(t, "hello", f.ParseFieldsValue(`"hello"`))
		assert.Equal(t, "unquoted", f.ParseFieldsValue("unquoted"))
	})
}

func TestSecurityImprovementsIntegration(t *testing.T) {
	t.Run("CompleteSecurityWorkflow", func(t *testing.T) {
		// Create a figo instance with security features enabled
		f := New(RawAdapter{})

		// Set up field whitelist
		f.SetAllowedFields("id", "name", "email", "age", "created_at")
		f.EnableFieldWhitelist()

		// Set up query limits
		f.SetQueryLimits(QueryLimits{
			MaxNestingDepth:    5,
			MaxFieldCount:      10,
			MaxParameterCount:  20,
			MaxExpressionCount: 50,
		})

		// Test complex query with mixed data types
		dsl := `id=1 and name="John" and email=^"%@gmail.com" and age>18 and created_at>"2023-01-01" and password="secret"`
		f.AddFiltersFromString(dsl)
		f.Build()

		// Verify that only whitelisted fields are processed
		clauses := f.GetClauses()
		fieldNames := make(map[string]bool)

		// Helper function to extract field names from expressions
		var extractFields func(expr Expr)
		extractFields = func(expr Expr) {
			switch c := expr.(type) {
			case EqExpr:
				fieldNames[c.Field] = true
			case GtExpr:
				fieldNames[c.Field] = true
			case LikeExpr:
				fieldNames[c.Field] = true
			case AndExpr:
				for _, operand := range c.Operands {
					extractFields(operand)
				}
			case OrExpr:
				for _, operand := range c.Operands {
					extractFields(operand)
				}
			}
		}

		for _, clause := range clauses {
			extractFields(clause)
		}

		// Should contain only whitelisted fields
		assert.True(t, fieldNames["id"])
		assert.True(t, fieldNames["name"])
		assert.True(t, fieldNames["email"])
		assert.True(t, fieldNames["age"])
		assert.True(t, fieldNames["created_at"])

		// Should not contain disallowed fields
		assert.False(t, fieldNames["password"])

		// Verify limits are respected
		limits := f.GetQueryLimits()
		assert.Equal(t, 5, limits.MaxNestingDepth)
		assert.Equal(t, 10, limits.MaxFieldCount)
	})

	t.Run("FieldWhitelistWithDifferentAdapters", func(t *testing.T) {
		adapters := []Adapter{
			RawAdapter{},
			GormAdapter{},
			MongoAdapter{},
			ElasticsearchAdapter{},
		}

		for _, adapter := range adapters {
			t.Run("Adapter", func(t *testing.T) {
				f := New(adapter)
				f.SetAllowedFields("id", "name")
				f.EnableFieldWhitelist()

				f.AddFiltersFromString(`id=1 and name="test" and password="secret"`)
				f.Build()

				// Should only process whitelisted fields
				clauses := f.GetClauses()
				assert.LessOrEqual(t, len(clauses), 2) // Only id and name
			})
		}
	})
}

func TestParseError(t *testing.T) {
	t.Run("ParseErrorFormatting", func(t *testing.T) {
		err := &ParseError{
			Message:  "Invalid syntax",
			Position: 10,
			Line:     2,
			Column:   5,
			Context:  "id=1 and invalid",
		}

		expected := "Parse error at line 2, column 5: Invalid syntax"
		assert.Equal(t, expected, err.Error())
	})

	t.Run("ParseErrorWithoutLine", func(t *testing.T) {
		err := &ParseError{
			Message:  "Invalid syntax",
			Position: 10,
		}

		expected := "Parse error at position 10: Invalid syntax"
		assert.Equal(t, expected, err.Error())
	})
}

func TestBackwardCompatibility(t *testing.T) {
	t.Run("ExistingFunctionalityUnchanged", func(t *testing.T) {
		// Test that existing functionality still works
		f := New(RawAdapter{})

		// Test basic functionality
		f.AddFiltersFromString(`id=1 and name="test"`)
		f.Build()

		clauses := f.GetClauses()
		assert.Len(t, clauses, 1) // Should be a single AndExpr clause

		// Test that new features are optional
		assert.True(t, f.IsFieldAllowed("any_field")) // Should return true when whitelist is disabled
		limits := f.GetQueryLimits()
		assert.Equal(t, 10, limits.MaxNestingDepth) // Should have default limits
	})
}
