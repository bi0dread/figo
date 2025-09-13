package figo

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Test Plugin System
func TestPluginSystem(t *testing.T) {
	t.Run("PluginManager", func(t *testing.T) {
		manager := NewPluginManager()

		// Test plugin registration
		plugin := &TestPlugin{name: "test-plugin", version: "1.0.0"}
		err := manager.RegisterPlugin(plugin)
		assert.NoError(t, err)

		// Test duplicate registration
		err = manager.RegisterPlugin(plugin)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already registered")

		// Test plugin retrieval
		retrieved, exists := manager.GetPlugin("test-plugin")
		assert.True(t, exists)
		assert.Equal(t, "test-plugin", retrieved.Name())

		// Test plugin listing
		plugins := manager.ListPlugins()
		assert.Len(t, plugins, 1)
		assert.Equal(t, "test-plugin", plugins[0].Name())

		// Test plugin unregistration
		err = manager.UnregisterPlugin("test-plugin")
		assert.NoError(t, err)

		// Test unregistration of non-existent plugin
		err = manager.UnregisterPlugin("non-existent")
		assert.Error(t, err)
	})

	t.Run("FigoPluginIntegration", func(t *testing.T) {
		f := New(RawAdapter{})

		// Test plugin registration
		plugin := &TestPlugin{name: "test-plugin", version: "1.0.0"}
		err := f.RegisterPlugin(plugin)
		assert.NoError(t, err)

		// Test plugin manager retrieval
		manager := f.GetPluginManager()
		assert.NotNil(t, manager)

		// Test plugin execution manually
		f.AddFiltersFromString(`id=1`)

		// Execute plugin hooks manually
		modifiedDSL, err := manager.ExecuteBeforeParse(f, `id=1`)
		assert.NoError(t, err)
		assert.Equal(t, `id=1`, modifiedDSL)

		err = manager.ExecuteAfterParse(f, `id=1`)
		assert.NoError(t, err)

		f.Build()

		// Verify plugin was called
		assert.True(t, plugin.initialized)
		assert.True(t, plugin.beforeParseCalled)
		assert.True(t, plugin.afterParseCalled)
	})

	t.Run("PluginHooks", func(t *testing.T) {
		manager := NewPluginManager()
		plugin := &TestPlugin{name: "hook-test", version: "1.0.0"}
		manager.RegisterPlugin(plugin)

		f := New(RawAdapter{})
		f.SetPluginManager(manager)

		// Test BeforeQuery hook
		err := manager.ExecuteBeforeQuery(f, nil)
		assert.NoError(t, err)
		assert.True(t, plugin.beforeQueryCalled)

		// Test AfterQuery hook
		err = manager.ExecuteAfterQuery(f, nil, "test result")
		assert.NoError(t, err)
		assert.True(t, plugin.afterQueryCalled)

		// Test BeforeParse hook
		modifiedDSL, err := manager.ExecuteBeforeParse(f, "id=1")
		assert.NoError(t, err)
		assert.Equal(t, "id=1", modifiedDSL)
		assert.True(t, plugin.beforeParseCalled)

		// Test AfterParse hook
		err = manager.ExecuteAfterParse(f, "id=1")
		assert.NoError(t, err)
		assert.True(t, plugin.afterParseCalled)
	})
}

// Test Advanced Operators
func TestAdvancedOperators(t *testing.T) {
	t.Run("JsonPathExpr", func(t *testing.T) {
		expr := JsonPathExpr{
			Field: "metadata",
			Path:  "$.user.name",
			Value: "john",
			Op:    "=",
		}

		assert.Equal(t, "metadata", expr.Field)
		assert.Equal(t, "$.user.name", expr.Path)
		assert.Equal(t, "john", expr.Value)
		assert.Equal(t, "=", expr.Op)
	})

	t.Run("ArrayContainsExpr", func(t *testing.T) {
		expr := ArrayContainsExpr{
			Field:  "tags",
			Values: []any{"tech", "golang", "database"},
		}

		assert.Equal(t, "tags", expr.Field)
		assert.Len(t, expr.Values, 3)
		assert.Contains(t, expr.Values, "tech")
	})

	t.Run("ArrayOverlapsExpr", func(t *testing.T) {
		expr := ArrayOverlapsExpr{
			Field:  "categories",
			Values: []any{"business", "finance"},
		}

		assert.Equal(t, "categories", expr.Field)
		assert.Len(t, expr.Values, 2)
	})

	t.Run("FullTextSearchExpr", func(t *testing.T) {
		expr := FullTextSearchExpr{
			Field:    "content",
			Query:    "machine learning algorithms",
			Language: "en",
		}

		assert.Equal(t, "content", expr.Field)
		assert.Equal(t, "machine learning algorithms", expr.Query)
		assert.Equal(t, "en", expr.Language)
	})

	t.Run("GeoDistanceExpr", func(t *testing.T) {
		expr := GeoDistanceExpr{
			Field:     "location",
			Latitude:  40.7128,
			Longitude: -74.0060,
			Distance:  10.0,
			Unit:      "km",
		}

		assert.Equal(t, "location", expr.Field)
		assert.Equal(t, 40.7128, expr.Latitude)
		assert.Equal(t, -74.0060, expr.Longitude)
		assert.Equal(t, 10.0, expr.Distance)
		assert.Equal(t, "km", expr.Unit)
	})

	t.Run("CustomExpr", func(t *testing.T) {
		handler := func(field, operator string, value any) (string, []any, error) {
			return "custom_query", []any{value}, nil
		}

		expr := CustomExpr{
			Field:    "custom_field",
			Operator: "custom_op",
			Value:    "custom_value",
			Handler:  handler,
		}

		assert.Equal(t, "custom_field", expr.Field)
		assert.Equal(t, "custom_op", expr.Operator)
		assert.Equal(t, "custom_value", expr.Value)
		assert.NotNil(t, expr.Handler)
	})
}

// Test Validation System
func TestValidationSystem(t *testing.T) {
	t.Run("ValidationManager", func(t *testing.T) {
		manager := NewValidationManager()

		// Test rule addition
		rule := ValidationRule{
			Field:   "email",
			Rule:    "email",
			Message: "Invalid email format",
		}
		manager.AddRule(rule)

		// Test validator registration
		validator := EmailValidator{}
		manager.RegisterValidator(validator)

		// Test validation
		err := manager.Validate("email", "invalid-email")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Invalid email format")

		err = manager.Validate("email", "valid@example.com")
		assert.NoError(t, err)
	})

	t.Run("BuiltInValidators", func(t *testing.T) {
		// Test RequiredValidator
		required := RequiredValidator{}
		err := required.Validate("name", "required", "")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "is required")

		err = required.Validate("name", "required", "john")
		assert.NoError(t, err)

		// Test MinLengthValidator
		minLength := MinLengthValidator{}
		err = minLength.Validate("name", "min_length", "ab")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "at least 3 characters")

		err = minLength.Validate("name", "min_length", "john")
		assert.NoError(t, err)

		// Test EmailValidator
		email := EmailValidator{}
		err = email.Validate("email", "email", "invalid")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must be a valid email")

		err = email.Validate("email", "email", "test@example.com")
		assert.NoError(t, err)
	})

	t.Run("FigoValidationIntegration", func(t *testing.T) {
		f := New(RawAdapter{})

		// Test validation manager setup
		manager := NewValidationManager()
		manager.RegisterValidator(EmailValidator{})
		manager.RegisterValidator(RequiredValidator{})

		manager.AddRule(ValidationRule{
			Field:   "email",
			Rule:    "email",
			Message: "Invalid email format",
		})

		f.SetValidationManager(manager)

		// Test field validation
		err := f.ValidateField("email", "invalid-email")
		assert.Error(t, err)

		err = f.ValidateField("email", "valid@example.com")
		assert.NoError(t, err)

		// Test validation rule addition
		f.AddValidationRule(ValidationRule{
			Field:   "name",
			Rule:    "required",
			Message: "Name is required",
		})

		err = f.ValidateField("name", "")
		assert.Error(t, err)

		err = f.ValidateField("name", "john")
		assert.NoError(t, err)
	})

	t.Run("CustomValidationRule", func(t *testing.T) {
		manager := NewValidationManager()

		// Custom validation rule
		rule := ValidationRule{
			Field:   "age",
			Rule:    "min_age",
			Message: "Age must be at least 18",
			Handler: func(field, rule string, value any) error {
				if age, ok := value.(int); ok {
					if age < 18 {
						return fmt.Errorf("age must be at least 18")
					}
				}
				return nil
			},
		}

		manager.AddRule(rule)

		// Test validation
		err := manager.Validate("age", 16)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Age must be at least 18")

		err = manager.Validate("age", 25)
		assert.NoError(t, err)
	})
}

// Test Integration of All Phase 3 Features
func TestPhase3Integration(t *testing.T) {
	t.Run("CompleteAdvancedWorkflow", func(t *testing.T) {
		// Create figo instance with all Phase 3 features
		f := New(RawAdapter{})

		// Set up plugin system
		plugin := &TestPlugin{name: "integration-test", version: "1.0.0"}
		err := f.RegisterPlugin(plugin)
		assert.NoError(t, err)

		// Set up validation system
		validationManager := NewValidationManager()
		validationManager.RegisterValidator(EmailValidator{})
		validationManager.AddRule(ValidationRule{
			Field:   "email",
			Rule:    "email",
			Message: "Invalid email format",
		})
		f.SetValidationManager(validationManager)

		// Test validation
		err = f.ValidateField("email", "test@example.com")
		assert.NoError(t, err)

		// Test plugin execution
		f.AddFiltersFromString(`id=1`)

		// Execute plugin hooks manually
		manager := f.GetPluginManager()
		modifiedDSL, err := manager.ExecuteBeforeParse(f, `id=1`)
		assert.NoError(t, err)
		assert.Equal(t, `id=1`, modifiedDSL)

		err = manager.ExecuteAfterParse(f, `id=1`)
		assert.NoError(t, err)

		f.Build()

		// Verify plugin was called
		assert.True(t, plugin.initialized)
		assert.True(t, plugin.beforeParseCalled)
		assert.True(t, plugin.afterParseCalled)

		// Test advanced operators
		jsonExpr := JsonPathExpr{
			Field: "metadata",
			Path:  "$.user.name",
			Value: "john",
			Op:    "=",
		}
		f.AddFilter(jsonExpr)

		// Test array operations
		arrayExpr := ArrayContainsExpr{
			Field:  "tags",
			Values: []any{"tech", "golang"},
		}
		f.AddFilter(arrayExpr)

		// Verify expressions were added
		clauses := f.GetClauses()
		assert.Len(t, clauses, 3) // id=1, jsonExpr, arrayExpr
	})

	t.Run("PluginErrorHandling", func(t *testing.T) {
		manager := NewPluginManager()
		errorPlugin := &ErrorPlugin{name: "error-plugin", version: "1.0.0"}
		manager.RegisterPlugin(errorPlugin)

		f := New(RawAdapter{})
		f.SetPluginManager(manager)

		// Test plugin error handling
		err := manager.ExecuteBeforeQuery(f, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "BeforeQuery error")
	})
}

// Test Helper Types

// TestPlugin is a test implementation of the Plugin interface
type TestPlugin struct {
	name              string
	version           string
	initialized       bool
	beforeQueryCalled bool
	afterQueryCalled  bool
	beforeParseCalled bool
	afterParseCalled  bool
}

func (p *TestPlugin) Name() string    { return p.name }
func (p *TestPlugin) Version() string { return p.version }
func (p *TestPlugin) Initialize(f Figo) error {
	p.initialized = true
	return nil
}
func (p *TestPlugin) BeforeQuery(f Figo, ctx any) error {
	p.beforeQueryCalled = true
	return nil
}
func (p *TestPlugin) AfterQuery(f Figo, ctx any, result interface{}) error {
	p.afterQueryCalled = true
	return nil
}
func (p *TestPlugin) BeforeParse(f Figo, dsl string) (string, error) {
	p.beforeParseCalled = true
	return dsl, nil
}
func (p *TestPlugin) AfterParse(f Figo, dsl string) error {
	p.afterParseCalled = true
	return nil
}

// ErrorPlugin is a test plugin that returns errors
type ErrorPlugin struct {
	name    string
	version string
}

func (p *ErrorPlugin) Name() string            { return p.name }
func (p *ErrorPlugin) Version() string         { return p.version }
func (p *ErrorPlugin) Initialize(f Figo) error { return nil }
func (p *ErrorPlugin) BeforeQuery(f Figo, ctx any) error {
	return fmt.Errorf("BeforeQuery error")
}
func (p *ErrorPlugin) AfterQuery(f Figo, ctx any, result interface{}) error {
	return fmt.Errorf("AfterQuery error")
}
func (p *ErrorPlugin) BeforeParse(f Figo, dsl string) (string, error) {
	return "", fmt.Errorf("BeforeParse error")
}
func (p *ErrorPlugin) AfterParse(f Figo, dsl string) error {
	return fmt.Errorf("AfterParse error")
}
