package plugins

// Integration tests moved from the core package when the built-in plugins
// were split into their own package. The dot-import keeps the original test
// bodies unchanged: core figo API is unqualified, plugin API is local.

import (
	"fmt"
	. "github.com/bi0dread/figo/v4/adapters"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/bi0dread/figo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestAddBanFields(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("sensitive_field", "internal_use_only")
	assert.True(t, fp.GetIgnoreFields()["sensitive_field"])
	assert.True(t, fp.GetIgnoreFields()["internal_use_only"])
}

func TestRawAdapterBuild(t *testing.T) {
	f := New()
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("bank_id")
	assert.NoError(t, f.RegisterPlugin(fp))
	f.AddFiltersFromString(`(id=1 and vendorId="22") and bank_id=11 or expedition_type=^"%e%" sort=id:desc page=skip:0,take:10`)
	f.Build(RawAdapter{})

	sql, args, _ := BuildRawSelect(f, "test_models")
	// Precedence gives ((id=1 AND vendor_id=22) AND bank_id=11) OR expedition_type LIKE %e%;
	// pruning the ignored bank_id from the conjunction must keep the OR:
	// (id=1 AND vendor_id=22) OR expedition_type LIKE %e%
	assert.Equal(t, "SELECT * FROM `test_models` WHERE ((`id` = ? AND `vendor_id` = ?) OR `expedition_type` LIKE ?) ORDER BY `id` DESC LIMIT 10", sql)
	// vendorId="22" is quoted, so it stays the string "22".
	assert.Equal(t, []any{int64(1), "22", "%e%"}, args)
}

func TestMongoAdapterBuild(t *testing.T) {
	f := New()
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("bank_id")
	assert.NoError(t, f.RegisterPlugin(fp))
	f.AddFiltersFromString(`(id=1 and vendorId="22") and bank_id=11 or expedition_type=^"%e%" sort=id:desc page=skip:0,take:10`)
	f.Build(nil)

	// Filter
	filter, _ := BuildMongoFilter(f)

	// Precedence gives ((id=1 AND vendor_id=22) AND bank_id=11) OR expedition_type LIKE %e%;
	// pruning the ignored bank_id from the conjunction must keep the OR.
	// This creates a top-level $or with two items:
	// 1. A nested $and with id and vendor_id
	// 2. expedition_type with regex
	orVal, ok := filter["$or"].([]bson.M)
	assert.True(t, ok)
	assert.Len(t, orVal, 2)

	// First item should be a nested $and with id and vendor_id
	firstItem, ok := orVal[0]["$and"].([]bson.M)
	assert.True(t, ok)
	assert.Len(t, firstItem, 2)

	// Verify id and vendor_id are present
	var hasID, hasVendor bool
	for _, m := range firstItem {
		if v, ok := m["id"]; ok && v == int64(1) {
			hasID = true
		}
		if v, ok := m["vendor_id"]; ok && v == "22" { // quoted literal stays a string
			hasVendor = true
		}
	}
	assert.True(t, hasID)
	assert.True(t, hasVendor)

	// Second item should be expedition_type with regex
	secondItem := orVal[1]
	if rv, ok := secondItem["expedition_type"].(primitive.Regex); ok {
		assert.NotEmpty(t, rv.Pattern)
	} else {
		t.Fatalf("expedition_type regex not found in filter")
	}

	// Options
	opts := BuildMongoFindOptions(f)
	if opts.Limit == nil || *opts.Limit != int64(10) {
		t.Fatalf("limit not set to 10")
	}
	if opts.Skip != nil {
		t.Fatalf("skip should be nil for 0")
	}
	if sd, ok := opts.Sort.(bson.D); ok {
		assert.Len(t, sd, 1)
		assert.Equal(t, "id", sd[0].Key)
		assert.Equal(t, -1, sd[0].Value)
	} else {
		t.Fatalf("sort not set as bson.D")
	}

	// Preloads to joins
	f2 := New()
	f2.AddFiltersFromString(`load=[TestInner1:id="3" or name="test1" | TestInner2:id=4]`)
	f2.Build(nil)
	joins := map[string]MongoJoin{
		"TestInner1": {From: "testinner1", LocalField: "id", ForeignField: "XX", As: "TestInner1"},
		"TestInner2": {From: "testinner2", LocalField: "id", ForeignField: "XX", As: "TestInner2"},
	}
	pipe, _ := BuildMongoAggregatePipeline(f2, joins)
	// Expect at least two $lookup stages
	lookupCount := 0
	matchQualified := 0
	for _, stage := range pipe {
		var lookupVal any
		var matchVal any
		for _, e := range stage { // stage is a bson.D
			switch e.Key {
			case "$lookup":
				lookupVal = e.Value
			case "$match":
				matchVal = e.Value
			}
		}
		if lookupVal != nil {
			lookupCount++
		}
		if matchVal != nil {
			if mm, ok := matchVal.(bson.M); ok {
				// look for qualified keys
				for k := range mm {
					if strings.HasPrefix(k, "TestInner1.") || strings.HasPrefix(k, "TestInner2.") {
						matchQualified++
						break
					}
				}
			}
		}
	}
	assert.Equal(t, 2, lookupCount)
	assert.True(t, matchQualified >= 1)
}

// Test Security Features
func TestFieldWhitelist(t *testing.T) {
	t.Run("FieldWhitelistEnabled", func(t *testing.T) {
		f := New()
		fp := NewFieldsPlugin()
		fp.SetAllowedFields("id", "name", "email")
		fp.EnableFieldWhitelist()
		assert.NoError(t, f.RegisterPlugin(fp))

		// Test allowed fields
		assert.True(t, fp.IsFieldAllowed("id"))
		assert.True(t, fp.IsFieldAllowed("name"))
		assert.True(t, fp.IsFieldAllowed("email"))

		// Test disallowed fields
		assert.False(t, fp.IsFieldAllowed("password"))
		assert.False(t, fp.IsFieldAllowed("secret"))

		// Test with DSL
		f.AddFiltersFromString(`id=1 and name="test" and password="secret"`)
		f.Build(RawAdapter{})
		clauses := f.GetClauses()
		// Should only have clauses for allowed fields
		assert.Len(t, clauses, 1) // Only the AndExpr.Expr with id and name
	})

	t.Run("FieldWhitelistDisabled", func(t *testing.T) {
		fp := NewFieldsPlugin()
		fp.SetAllowedFields("id", "name")
		// Don't enable whitelist

		// All fields should be allowed when whitelist is disabled
		assert.True(t, fp.IsFieldAllowed("password"))
		assert.True(t, fp.IsFieldAllowed("secret"))
	})

	t.Run("FieldWhitelistWithDSL", func(t *testing.T) {
		f := New()
		fp := NewFieldsPlugin()
		fp.SetAllowedFields("id", "name")
		fp.EnableFieldWhitelist()
		assert.NoError(t, f.RegisterPlugin(fp))

		f.AddFiltersFromString(`id=1 and name="test" and password="secret"`)
		f.Build(RawAdapter{})

		// Should filter out disallowed fields
		clauses := f.GetClauses()
		assert.Len(t, clauses, 1) // Only allowed fields remain
	})
}

func TestQueryLimitsBasic(t *testing.T) {
	t.Run("DefaultLimits", func(t *testing.T) {
		limits := DefaultQueryLimits()

		assert.Equal(t, 10, limits.MaxNestingDepth)
		assert.Equal(t, 50, limits.MaxFieldCount)
		assert.Equal(t, 100, limits.MaxParameterCount)
		assert.Equal(t, 200, limits.MaxExpressionCount)
	})

	t.Run("CustomLimits", func(t *testing.T) {
		lp := NewLimitsPlugin(QueryLimits{
			MaxNestingDepth:    5,
			MaxFieldCount:      10,
			MaxParameterCount:  20,
			MaxExpressionCount: 5,
		})

		limits := lp.GetLimits()
		assert.Equal(t, 5, limits.MaxNestingDepth)
		assert.Equal(t, 10, limits.MaxFieldCount)
		assert.Equal(t, 20, limits.MaxParameterCount)
		assert.Equal(t, 5, limits.MaxExpressionCount)
	})

	t.Run("LimitsEnforcedOnParse", func(t *testing.T) {
		f := New()
		lp := NewLimitsPlugin(QueryLimits{MaxExpressionCount: 2})
		assert.NoError(t, f.RegisterPlugin(lp))

		assert.NoError(t, f.AddFiltersFromString(`a=1`))

		err := f.AddFiltersFromString(`a=1 and b=2 and c=3`)
		assert.Error(t, err, "query over MaxExpressionCount must fail the parse")
		assert.Contains(t, err.Error(), "MaxExpressionCount")
	})
}

func TestInputValidation(t *testing.T) {
	t.Run("ValidInput", func(t *testing.T) {
		f := New()
		err := f.AddFiltersFromString(`id=1 and name="test"`)
		assert.NoError(t, err)
	})

	t.Run("UnmatchedParentheses", func(t *testing.T) {
		// The strict syntax plugin rejects malformed input via BeforeParse.
		f := New()
		assert.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(false)))
		err := f.AddFiltersFromString(`(id=1 and name="test" and (age > 25 and (status = "active"`)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unmatched")
	})

	t.Run("UnmatchedQuotes", func(t *testing.T) {
		f := New()
		assert.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(false)))
		err := f.AddFiltersFromString(`id=1 and name="test and age > 25 and status = "active"`)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unmatched")
	})

	t.Run("RepairFixesTrailingParen", func(t *testing.T) {
		// With repair enabled, a missing closing parenthesis is fixed and
		// the DSL parses.
		f := New()
		assert.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(true)))
		assert.NoError(t, f.AddFiltersFromString(`(name="john" and age>25`))
		f.Build(RawAdapter{})
		assert.Len(t, f.GetClauses(), 1)
	})

	t.Run("EmptyInput", func(t *testing.T) {
		f := New()
		err := f.AddFiltersFromString("")
		assert.NoError(t, err)
	})

	t.Run("WhitespaceOnly", func(t *testing.T) {
		f := New()
		err := f.AddFiltersFromString("   ")
		assert.NoError(t, err)
	})
}

// Test Performance Features
func TestCaching(t *testing.T) {
	t.Run("InMemoryCache", func(t *testing.T) {
		cache := NewInMemoryCache(CacheConfig{
			Enabled:         true,
			MaxSize:         100,
			TTL:             5 * time.Minute,
			CleanupInterval: 1 * time.Minute,
		})
		defer cache.Close()

		// Test Set and Get
		cache.Set("key1", "value1", 5*time.Minute)
		value, found := cache.Get("key1")
		assert.True(t, found)
		assert.Equal(t, "value1", value)

		// Test Get non-existent key
		_, found = cache.Get("nonexistent")
		assert.False(t, found)

		// Test Set with TTL (kept short: this sleep is pure wall-clock cost)
		cache.Set("key2", "value2", 50*time.Millisecond)
		value, found = cache.Get("key2")
		assert.True(t, found)
		assert.Equal(t, "value2", value)

		// Wait for expiration
		time.Sleep(120 * time.Millisecond)
		_, found = cache.Get("key2")
		assert.False(t, found)
	})

	t.Run("CacheStats", func(t *testing.T) {
		cache := NewInMemoryCache(CacheConfig{
			Enabled:         true,
			MaxSize:         10,
			TTL:             5 * time.Minute,
			CleanupInterval: 1 * time.Minute,
		})
		defer cache.Close()

		cache.Set("key1", "value1", 5*time.Minute)
		cache.Set("key2", "value2", 5*time.Minute)

		stats := cache.Stats()
		assert.Equal(t, int64(0), stats.Hits)   // No hits yet since we haven't retrieved
		assert.Equal(t, int64(0), stats.Misses) // No misses yet
		assert.Equal(t, 2, stats.Size)
	})
}

func TestPerformanceMonitor(t *testing.T) {
	t.Run("PerformanceMonitoring", func(t *testing.T) {
		monitor := NewPerformanceMonitor(true)

		// Record some queries
		monitor.RecordQuery(100*time.Millisecond, true, nil)
		monitor.RecordQuery(200*time.Millisecond, false, nil)
		monitor.RecordQuery(150*time.Millisecond, true, nil)

		// Get metrics
		metrics := monitor.GetMetrics()
		assert.Equal(t, int64(3), metrics.QueryCount)
		assert.Equal(t, int64(2), metrics.CacheHits)
		assert.Equal(t, int64(1), metrics.CacheMisses)
		assert.True(t, metrics.AverageLatency > 0)
		assert.True(t, metrics.TotalLatency > 0)
	})

	t.Run("PerformanceReset", func(t *testing.T) {
		monitor := NewPerformanceMonitor(true)

		monitor.RecordQuery(100*time.Millisecond, true, nil)
		monitor.RecordQuery(200*time.Millisecond, false, nil)

		metrics := monitor.GetMetrics()
		assert.Equal(t, int64(2), metrics.QueryCount)

		monitor.Reset()
		metrics = monitor.GetMetrics()
		assert.Equal(t, int64(0), metrics.QueryCount)
	})
}

// Test Advanced Operators (Basic functionality)

// Test Plugin System (Basic functionality)

// BeforeQuery/AfterQuery fire automatically on the render path.
func TestQueryHooksWiredIntoRender(t *testing.T) {
	newBuilt := func(hp *queryHookPlugin) Figo {
		f := New()
		assert.NoError(t, f.RegisterPlugin(hp))
		assert.NoError(t, f.AddFiltersFromString(`id=1`))
		f.Build(RawAdapter{})
		return f
	}

	t.Run("HooksFireOnBothRenderPaths", func(t *testing.T) {
		hp := &queryHookPlugin{}
		f := newBuilt(hp)

		sql := f.GetSqlString(RawContext{Table: "t"})
		assert.NotEmpty(t, sql)
		assert.Equal(t, 1, hp.beforeCalls)
		assert.Equal(t, 1, hp.afterCalls)
		assert.Equal(t, sql, hp.lastResult, "AfterQuery receives the rendered SQL")

		q := f.GetQuery(RawContext{Table: "t"})
		assert.NotNil(t, q)
		assert.Equal(t, 2, hp.beforeCalls)
		assert.Equal(t, 2, hp.afterCalls)
		assert.Equal(t, q, hp.lastResult, "AfterQuery receives the rendered Query")
	})

	t.Run("BeforeQueryErrorVetoesRender", func(t *testing.T) {
		hp := &queryHookPlugin{beforeErr: fmt.Errorf("not authorized")}
		f := newBuilt(hp)

		assert.Empty(t, f.GetSqlString(RawContext{Table: "t"}))
		assert.Nil(t, f.GetQuery(RawContext{Table: "t"}))
		assert.Zero(t, hp.afterCalls, "AfterQuery must not run when BeforeQuery vetoed")
	})

	t.Run("AfterQueryErrorVetoesResult", func(t *testing.T) {
		hp := &queryHookPlugin{afterErr: fmt.Errorf("rejected")}
		f := newBuilt(hp)

		assert.Empty(t, f.GetSqlString(RawContext{Table: "t"}))
		assert.Nil(t, f.GetQuery(RawContext{Table: "t"}))
	})

	t.Run("HooksFireOnCacheMissNotHit", func(t *testing.T) {
		hp := &queryHookPlugin{}
		f := newBuilt(hp)
		cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute, MaxSize: 10})
		defer cp.Close()

		_ = cp.GetCachedSqlString(f, RawContext{Table: "t"}) // miss -> renders
		assert.Equal(t, 1, hp.beforeCalls)
		_ = cp.GetCachedSqlString(f, RawContext{Table: "t"}) // hit -> no render
		assert.Equal(t, 1, hp.beforeCalls, "cache hits must not fire query hooks")
	})
}

// Test Concurrency Safety
func TestConcurrencySafety(t *testing.T) {
	t.Run("ConcurrentAccess", func(t *testing.T) {
		f := New()
		fp := NewFieldsPlugin()
		assert.NoError(t, f.RegisterPlugin(fp))
		var wg sync.WaitGroup
		numGoroutines := 10

		// Test concurrent field operations
		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				fp.AddIgnoreFields(fmt.Sprintf("field_%d", id))
				f.AddSelectFields(fmt.Sprintf("select_%d", id))
				fp.SetAllowedFields(fmt.Sprintf("allowed_%d", id))
				fp.GetIgnoreFields()
				f.GetSelectFields()
				fp.GetAllowedFields()
			}(i)
		}

		wg.Wait()

		// Verify no race conditions occurred
		ignoreFields := fp.GetIgnoreFields()
		selectFields := f.GetSelectFields()
		allowedFields := fp.GetAllowedFields()

		assert.NotNil(t, ignoreFields)
		assert.NotNil(t, selectFields)
		assert.NotNil(t, allowedFields)
	})

	t.Run("ConcurrentBuild", func(t *testing.T) {
		f := New()
		var wg sync.WaitGroup
		numGoroutines := 5

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				f.AddFiltersFromString(fmt.Sprintf("id=%d", id))
				f.Build(RawAdapter{})
				f.GetClauses()
			}(i)
		}

		wg.Wait()

		// Should not panic and should have some clauses
		clauses := f.GetClauses()
		assert.NotNil(t, clauses)
	})
}

// Test Memory Management

// Test Memory Management
func TestMemoryManagement(t *testing.T) {
	t.Run("CacheMemoryLeak", func(t *testing.T) {
		cache := NewInMemoryCache(CacheConfig{
			MaxSize:         10,
			TTL:             100 * time.Millisecond,
			CleanupInterval: 50 * time.Millisecond,
		})

		// Add items
		for i := 0; i < 20; i++ {
			cache.Set(fmt.Sprintf("key_%d", i), fmt.Sprintf("value_%d", i), 100*time.Millisecond)
		}

		// Wait for cleanup
		time.Sleep(200 * time.Millisecond)

		// Close cache to stop goroutine
		cache.Close()

		// Should not have memory leaks
		stats := cache.Stats()
		assert.True(t, stats.Size <= 10) // Should be limited by MaxSize
	})

	t.Run("LargeDataSet", func(t *testing.T) {
		f := New()

		// Add many filters
		for i := 0; i < 1000; i++ {
			f.AddFilter(EqExpr{
				Field: fmt.Sprintf("field_%d", i),
				Value: fmt.Sprintf("value_%d", i),
			})
		}

		f.Build(RawAdapter{})
		clauses := f.GetClauses()
		assert.Len(t, clauses, 1000)
	})
}

// Test Error Recovery

func TestBackwardCompatibilityBasic(t *testing.T) {
	t.Run("ExistingFunctionalityUnchanged", func(t *testing.T) {
		// Test that existing functionality still works
		f := New()

		// Test basic functionality
		f.AddFiltersFromString(`id=1 and name="test"`)
		f.Build(RawAdapter{})

		clauses := f.GetClauses()
		assert.Len(t, clauses, 1) // Should be a single AndExpr.Expr clause

		// Policy features are optional plugins; a fresh unregistered plugin
		// allows everything and enforces nothing.
		fp := NewFieldsPlugin()
		assert.True(t, fp.IsFieldAllowed("any_field")) // whitelist disabled by default
		assert.Equal(t, 10, DefaultQueryLimits().MaxNestingDepth)
	})

	t.Run("FieldSetters", func(t *testing.T) {
		f := New()
		fp := NewFieldsPlugin()

		fp.AddIgnoreFields("field1", "field2")
		f.AddSelectFields("field3", "field4")

		ignoreFields := fp.GetIgnoreFields()
		selectFields := f.GetSelectFields()

		assert.True(t, ignoreFields["field1"])
		assert.True(t, ignoreFields["field2"])
		assert.True(t, selectFields["field3"])
		assert.True(t, selectFields["field4"])
	})
}

// Test Validation System
func TestValidationSystem(t *testing.T) {
	t.Run("ValidationPlugin", func(t *testing.T) {
		plugin := NewValidationPlugin()

		// Test rule addition
		rule := ValidationRule{
			Field:   "email",
			Rule:    "email",
			Message: "Invalid email format",
		}
		plugin.AddRule(rule)

		// Test validator registration
		validator := EmailValidator{}
		plugin.RegisterValidator(validator)

		// Test validation
		err := plugin.Validate("email", "invalid-email")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Invalid email format")

		err = plugin.Validate("email", "valid@example.com")
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
		f := New()

		// Validation is wired in as a plugin
		plugin := NewValidationPlugin()
		plugin.RegisterValidator(EmailValidator{})
		plugin.RegisterValidator(RequiredValidator{})

		plugin.AddRule(ValidationRule{
			Field:   "email",
			Rule:    "email",
			Message: "Invalid email format",
		})

		err := f.RegisterPlugin(plugin)
		assert.NoError(t, err)

		// Parsing a DSL with an invalid value fails via the AfterParse hook
		err = f.AddFiltersFromString(`email="invalid-email"`)
		assert.Error(t, err)

		err = f.AddFiltersFromString(`email="valid@example.com"`)
		assert.NoError(t, err)

		// Direct validation stays available on the plugin
		err = plugin.Validate("email", "invalid-email")
		assert.Error(t, err)

		err = plugin.Validate("email", "valid@example.com")
		assert.NoError(t, err)

		plugin.AddRule(ValidationRule{
			Field:   "name",
			Rule:    "required",
			Message: "Name is required",
		})

		err = f.AddFiltersFromString(`name=""`)
		assert.Error(t, err)

		err = f.AddFiltersFromString(`name="john"`)
		assert.NoError(t, err)
	})

	t.Run("CustomValidationRule", func(t *testing.T) {
		manager := NewValidationPlugin()

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

// Test Integration of All Phase 3 Features
func TestPhase3Integration(t *testing.T) {
	t.Run("CompleteAdvancedWorkflow", func(t *testing.T) {
		// Create figo instance with all Phase 3 features
		f := New()

		// Set up plugin system
		plugin := &TestPlugin{name: "integration-test", version: "1.0.0"}
		err := f.RegisterPlugin(plugin)
		assert.NoError(t, err)

		// Set up validation as a plugin
		validationPlugin := NewValidationPlugin()
		validationPlugin.RegisterValidator(EmailValidator{})
		validationPlugin.AddRule(ValidationRule{
			Field:   "email",
			Rule:    "email",
			Message: "Invalid email format",
		})
		err = f.RegisterPlugin(validationPlugin)
		assert.NoError(t, err)

		// Test validation
		err = validationPlugin.Validate("email", "test@example.com")
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

		f.Build(RawAdapter{})

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

		f := New()
		f.SetPluginManager(manager)

		// Test plugin error handling
		err := manager.ExecuteBeforeQuery(f, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "BeforeQuery error")
	})
}

// Test Helper Types

// TestPlugin is a test implementation of the Plugin interface

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
		f := New()

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

		f.Build(RawAdapter{})

		// Verify plugin was called
		assert.True(t, plugin.initialized)
		assert.True(t, plugin.beforeParseCalled)
		assert.True(t, plugin.afterParseCalled)
	})

	t.Run("PluginHooks", func(t *testing.T) {
		manager := NewPluginManager()
		plugin := &TestPlugin{name: "hook-test", version: "1.0.0"}
		manager.RegisterPlugin(plugin)

		f := New()
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

// Dropping an ignored field must remove its condition from the logical
// structure without leaving the neighboring NOT/AND/OR to re-bind elsewhere.
func TestIgnoreFieldsDoNotOrphanOperators(t *testing.T) {
	t.Run("not on ignored field does not retarget", func(t *testing.T) {
		f := New()
		fp := NewFieldsPlugin()
		fp.AddIgnoreFields("secret")
		require.NoError(t, f.RegisterPlugin(fp))
		require.NoError(t, f.AddFiltersFromString(`not secret=1 and a=2`))
		f.Build(RawAdapter{})

		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		eq, ok := clauses[0].(EqExpr)
		require.True(t, ok, "expected bare EqExpr on a, got %#v", clauses[0])
		assert.Equal(t, "a", eq.Field)
	})

	t.Run("or survives an ignored middle condition", func(t *testing.T) {
		f := New()
		fp := NewFieldsPlugin()
		fp.AddIgnoreFields("secret")
		require.NoError(t, f.RegisterPlugin(fp))
		require.NoError(t, f.AddFiltersFromString(`a=1 and secret=2 or b=3`))
		f.Build(RawAdapter{})

		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		or, ok := clauses[0].(OrExpr)
		require.True(t, ok, "OR must survive, got %#v", clauses[0])
		assert.Len(t, or.Operands, 2)
	})

	t.Run("ignore matches across naming strategies", func(t *testing.T) {
		f := New() // default snake_case strategy
		fp := NewFieldsPlugin()
		fp.AddIgnoreFields("user_name")
		require.NoError(t, f.RegisterPlugin(fp))
		require.NoError(t, f.AddFiltersFromString(`userName=1 and a=2`))
		f.Build(RawAdapter{})

		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		eq, ok := clauses[0].(EqExpr)
		require.True(t, ok, "expected bare EqExpr on a, got %#v", clauses[0])
		assert.Equal(t, "a", eq.Field)
	})

	t.Run("ignored fields pruned from preloads", func(t *testing.T) {
		f := New()
		fp := NewFieldsPlugin()
		fp.AddIgnoreFields("secret")
		require.NoError(t, f.RegisterPlugin(fp))
		require.NoError(t, f.AddFiltersFromString(`a=1 load=[Orders:secret=1]`))
		f.Build(RawAdapter{})

		assert.Empty(t, f.GetPreloads()["Orders"])
	})
}

// A leading NOT is valid and must survive all three entry paths unchanged.

// A leading NOT is valid and must survive all three entry paths unchanged.
func TestLeadingNotNeverStripped(t *testing.T) {
	assertNegated := func(t *testing.T, f Figo) {
		t.Helper()
		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		not, ok := clauses[0].(NotExpr)
		require.True(t, ok, "expected NotExpr, got %#v", clauses[0])
		require.Len(t, not.Operands, 1)
		eq, ok := not.Operands[0].(EqExpr)
		require.True(t, ok)
		assert.Equal(t, "deleted", eq.Field)
	}

	t.Run("plain", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`not deleted=true`))
		f.Build(RawAdapter{})
		assertNegated(t, f)
	})

	t.Run("with repair plugin", func(t *testing.T) {
		f := New()
		require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(true)))
		require.NoError(t, f.AddFiltersFromString(`not deleted=true`))
		f.Build(RawAdapter{})
		assertNegated(t, f)
	})

	t.Run("with strict syntax plugin (validation)", func(t *testing.T) {
		f := New()
		require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(false)))
		require.NoError(t, f.AddFiltersFromString(`not deleted=true`))
		f.Build(RawAdapter{})
		assertNegated(t, f)
	})
}

// AddFilter must respect the whitelist and ignore-fields, including for the
// advanced expression types.
func TestAddFilterRespectsWhitelistAndIgnores(t *testing.T) {
	f := New()
	fp := NewFieldsPlugin()
	fp.SetAllowedFields("name")
	fp.EnableFieldWhitelist()
	require.NoError(t, f.RegisterPlugin(fp))
	f.AddFilter(EqExpr{Field: "secret", Value: 1})
	f.AddFilter(GeoDistanceExpr{Field: "hidden_location", Latitude: 1, Longitude: 2, Distance: 3})
	f.AddFilter(EqExpr{Field: "name", Value: "x"})
	require.Len(t, f.GetClauses(), 1)
	assert.Equal(t, "name", ExprField(f.GetClauses()[0]))

	f2 := New()
	fp2 := NewFieldsPlugin()
	fp2.AddIgnoreFields("secret")
	require.NoError(t, f2.RegisterPlugin(fp2))
	f2.AddFilter(NotExpr{Operands: []Expr{EqExpr{Field: "secret", Value: 1}}})
	assert.Empty(t, f2.GetClauses(), "ignored field must not enter via AddFilter")
}

// #2: the cache key must distinguish numeric value types; int64(1), float64(1)
// and int(1) must not collide when instances share a cache.
func TestRegr_CacheKeyDistinguishesNumericTypes(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute, MaxSize: 100})
	defer cp.Close()

	build := func(v any) SQLQuery {
		f := New()
		f.AddFilter(EqExpr{Field: "a", Value: v})
		f.Build(RawAdapter{})
		return cp.GetCachedQuery(f, "t").(SQLQuery)
	}

	qInt64 := build(int64(1))
	qFloat := build(float64(1))
	qInt := build(int(1))

	assert.IsType(t, int64(0), qInt64.Args[0])
	assert.IsType(t, float64(0), qFloat.Args[0], "float64 query must not return the cached int64 entry")
	assert.IsType(t, int(0), qInt.Args[0], "int query must not return a cached numeric entry of another type")
}

// #3: the MongoDB find adapter must honor AddSelectFields as a projection.

// #30: an expired entry is removed on access and no longer counted in Size.
func TestExpiredEntryDeletedOnGet(t *testing.T) {
	c := NewInMemoryCache(CacheConfig{Enabled: true, MaxSize: 10}) // no cleanup goroutine
	defer c.Close()
	c.Set("k", "v", time.Nanosecond)
	time.Sleep(time.Millisecond)

	_, ok := c.Get("k")
	assert.False(t, ok, "expired entry must be a miss")
	assert.Equal(t, 0, c.Stats().Size, "expired entry must be deleted, not linger")
}

// A validation handler may call back into the plugin without deadlocking.
func TestValidationHandlerMayCallManager(t *testing.T) {
	vm := NewValidationPlugin()
	vm.AddRule(ValidationRule{
		Field: "*",
		Rule:  "custom",
		Handler: func(field, rule string, value any) error {
			vm.AddRule(ValidationRule{Field: "other", Rule: "noop"})
			return nil
		},
	})

	done := make(chan struct{})
	go func() {
		_ = vm.Validate("x", 1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("validation handler calling AddRule deadlocked")
	}
}

// Dialect-divergent renders must not share a cache slot.
func TestCacheKeyDistinguishesDialects(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: 60_000_000_000, MaxSize: 10})
	defer cp.Close()

	f := New()
	require.NoError(t, f.AddFiltersFromString(`id=1`))
	f.Build(RawAdapter{})
	mysqlSQL := cp.GetCachedSqlString(f, RawContext{Table: "t"})
	assert.Contains(t, mysqlSQL, "`id`")

	f.Build(RawAdapter{Dialect: PostgresDialect})
	pgSQL := cp.GetCachedSqlString(f, RawContext{Table: "t"})
	assert.Contains(t, pgSQL, `"id"`, "postgres render must not be served from the mysql cache slot")
}

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

// queryHookPlugin records BeforeQuery/AfterQuery invocations and can veto.
type queryHookPlugin struct {
	beforeCalls int
	afterCalls  int
	lastResult  interface{}
	beforeErr   error
	afterErr    error
}

func (p *queryHookPlugin) Name() string          { return "query-hooks" }
func (p *queryHookPlugin) Version() string       { return "1.0.0" }
func (p *queryHookPlugin) Initialize(Figo) error { return nil }
func (p *queryHookPlugin) BeforeQuery(Figo, any) error {
	p.beforeCalls++
	return p.beforeErr
}
func (p *queryHookPlugin) AfterQuery(_ Figo, _ any, result interface{}) error {
	p.afterCalls++
	p.lastResult = result
	return p.afterErr
}
func (p *queryHookPlugin) BeforeParse(_ Figo, dsl string) (string, error) { return dsl, nil }
func (p *queryHookPlugin) AfterParse(Figo, string) error                  { return nil }
