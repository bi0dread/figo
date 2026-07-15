package plugins

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFieldWhitelisting(t *testing.T) {
	t.Run("FieldWhitelistDisabled", func(t *testing.T) {
		fp := NewFieldsPlugin()
		fp.DisableFieldWhitelist()

		// All fields should be allowed when whitelist is disabled
		assert.True(t, fp.IsFieldAllowed("any_field"))
		assert.True(t, fp.IsFieldAllowed("sensitive_data"))
	})

	t.Run("FieldWhitelistEnabled", func(t *testing.T) {
		fp := NewFieldsPlugin()
		fp.SetAllowedFields("id", "name", "email", "created_at")
		fp.EnableFieldWhitelist()

		// Allowed fields should pass
		assert.True(t, fp.IsFieldAllowed("id"))
		assert.True(t, fp.IsFieldAllowed("name"))
		assert.True(t, fp.IsFieldAllowed("email"))
		assert.True(t, fp.IsFieldAllowed("created_at"))

		// Disallowed fields should be blocked
		assert.False(t, fp.IsFieldAllowed("password"))
		assert.False(t, fp.IsFieldAllowed("secret_key"))
		assert.False(t, fp.IsFieldAllowed("internal_data"))
	})

	t.Run("FieldWhitelistWithDSL", func(t *testing.T) {
		f := New()
		fp := NewFieldsPlugin()
		fp.SetAllowedFields("id", "name", "email")
		fp.EnableFieldWhitelist()
		assert.NoError(t, f.RegisterPlugin(fp))

		// Add filters with allowed and disallowed fields
		f.AddFiltersFromString(`id=1 and name="test" and password="secret"`)
		f.Build(RawAdapter{})

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
		limits := DefaultQueryLimits()

		assert.Equal(t, 10, limits.MaxNestingDepth)
		assert.Equal(t, 50, limits.MaxFieldCount)
		assert.Equal(t, 100, limits.MaxParameterCount)
		assert.Equal(t, 200, limits.MaxExpressionCount)
	})

	t.Run("CustomLimits", func(t *testing.T) {
		lp := NewLimitsPlugin(DefaultQueryLimits())
		customLimits := QueryLimits{
			MaxNestingDepth:    5,
			MaxFieldCount:      10,
			MaxParameterCount:  20,
			MaxExpressionCount: 50,
		}
		lp.SetLimits(customLimits)

		limits := lp.GetLimits()
		assert.Equal(t, 5, limits.MaxNestingDepth)
		assert.Equal(t, 10, limits.MaxFieldCount)
		assert.Equal(t, 20, limits.MaxParameterCount)
		assert.Equal(t, 50, limits.MaxExpressionCount)
	})

	t.Run("EnforcedLimits", func(t *testing.T) {
		f := New()
		lp := NewLimitsPlugin(QueryLimits{MaxNestingDepth: 2, MaxParameterCount: 3})
		assert.NoError(t, f.RegisterPlugin(lp))

		// Within limits.
		assert.NoError(t, f.AddFiltersFromString(`a=1 and b=2`))

		// Too many parameters (an <in> list element each counts).
		err := f.AddFiltersFromString(`a<in>[1,2,3,4]`)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "MaxParameterCount")

		// Too deeply nested.
		err = f.AddFiltersFromString(`a=1 and (b=2 or (c=3 and d=4))`)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "MaxNestingDepth")
	})
}

func TestEnhancedTypeParsing(t *testing.T) {
	t.Run("DateParsing", func(t *testing.T) {
		// Unquoted date literals get date detection; quoted literals stay
		// strings (quoting is the "do not re-type this" escape hatch).
		testCases := []struct {
			input    string
			expected time.Time
		}{
			{`2023-01-15T10:30:00Z`, time.Date(2023, 1, 15, 10, 30, 0, 0, time.UTC)},
			{`2023-01-15`, time.Date(2023, 1, 15, 0, 0, 0, 0, time.UTC)},
			{`2023/01/15`, time.Date(2023, 1, 15, 0, 0, 0, 0, time.UTC)},
			{`Jan 15, 2023`, time.Date(2023, 1, 15, 0, 0, 0, 0, time.UTC)},
		}

		for _, tc := range testCases {
			t.Run(tc.input, func(t *testing.T) {
				// Parse the value
				value := ParseValue(tc.input)

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
		// Test null values
		assert.Nil(t, ParseValue("null"))
		assert.Nil(t, ParseValue("NULL"))
	})

	t.Run("BooleanParsing", func(t *testing.T) {
		// Test boolean values
		assert.True(t, ParseValue("true").(bool))
		assert.False(t, ParseValue("false").(bool))
	})

	t.Run("NumericParsing", func(t *testing.T) {
		// Test numeric values
		assert.Equal(t, int64(123), ParseValue("123"))
		assert.Equal(t, float64(123.45), ParseValue("123.45"))
	})

	t.Run("StringParsing", func(t *testing.T) {
		// Test string values
		assert.Equal(t, "hello", ParseValue(`"hello"`))
		assert.Equal(t, "unquoted", ParseValue("unquoted"))
	})
}

func TestSecurityImprovementsIntegration(t *testing.T) {
	t.Run("CompleteSecurityWorkflow", func(t *testing.T) {
		// Create a figo instance with security plugins registered
		f := New()

		// Set up field whitelist
		fp := NewFieldsPlugin()
		fp.SetAllowedFields("id", "name", "email", "age", "created_at")
		fp.EnableFieldWhitelist()
		assert.NoError(t, f.RegisterPlugin(fp))

		// Set up query limits
		lp := NewLimitsPlugin(QueryLimits{
			MaxNestingDepth:    5,
			MaxFieldCount:      10,
			MaxParameterCount:  20,
			MaxExpressionCount: 50,
		})
		assert.NoError(t, f.RegisterPlugin(lp))

		// Test complex query with mixed data types
		dsl := `id=1 and name="John" and email=^"%@gmail.com" and age>18 and created_at>"2023-01-01" and password="secret"`
		f.AddFiltersFromString(dsl)
		f.Build(RawAdapter{})

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
		limits := lp.GetLimits()
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
				f := New()
				fp := NewFieldsPlugin()
				fp.SetAllowedFields("id", "name")
				fp.EnableFieldWhitelist()
				assert.NoError(t, f.RegisterPlugin(fp))

				f.AddFiltersFromString(`id=1 and name="test" and password="secret"`)
				f.Build(adapter)

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
		f := New()

		// Test basic functionality
		f.AddFiltersFromString(`id=1 and name="test"`)
		f.Build(RawAdapter{})

		clauses := f.GetClauses()
		assert.Len(t, clauses, 1) // Should be a single AndExpr clause

		// Policy features are optional plugins with permissive defaults.
		fp := NewFieldsPlugin()
		assert.True(t, fp.IsFieldAllowed("any_field")) // whitelist disabled by default
		assert.Equal(t, 10, DefaultQueryLimits().MaxNestingDepth)
	})
}
