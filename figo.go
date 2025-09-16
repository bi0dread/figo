package figo

import (
	"context"
	"crypto/md5"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gobeam/stringy"
)

// Global configuration
var (
	// regexSQLOperator controls the SQL operator used for RegexExpr in SQL adapters.
	// Defaults to MySQL-compatible REGEXP. For Postgres, set to "~" or "~*".
	regexSQLOperator = "REGEXP"
)

// SetRegexSQLOperator sets the SQL operator used to render regex in SQL adapters (Raw/GORM)
func SetRegexSQLOperator(op string) {
	op = strings.TrimSpace(op)
	if op == "" {
		return
	}
	regexSQLOperator = op
}

// GetRegexSQLOperator returns the configured SQL regex operator
func GetRegexSQLOperator() string { return regexSQLOperator }

type NamingStrategy string

const NAMING_STRATEGY_NO_CHANGE = "no_change"
const NAMING_STRATEGY_SNAKE_CASE = "snake_case"

type Operation string

const (
	OperationEq       Operation = "="
	OperationGt       Operation = ">"
	OperationGte      Operation = ">="
	OperationLt       Operation = "<"
	OperationLte      Operation = "<="
	OperationNeq      Operation = "!="
	OperationNot      Operation = "not"
	OperationLike     Operation = "=^"
	OperationNotLike  Operation = "!=^"
	OperationRegex    Operation = "=~"
	OperationNotRegex Operation = "!=~"
	OperationAnd      Operation = "and"
	OperationOr       Operation = "or"
	OperationBetween  Operation = "<bet>"
	OperationIn       Operation = "<in>"
	OperationNotIn    Operation = "<nin>"
	OperationSort     Operation = "sort"
	OperationLoad     Operation = "load"
	OperationPage     Operation = "page"
	OperationChild    Operation = "----"
	OperationILike    Operation = ".=^"
	OperationIsNull   Operation = "<null>"
	OperationNotNull  Operation = "<notnull>"
)

// AdapterType removed: adapters are selected via Adapter objects

type Page struct {
	Skip int
	Take int
}

// QueryLimits defines limits for query complexity
type QueryLimits struct {
	MaxNestingDepth    int
	MaxFieldCount      int
	MaxParameterCount  int
	MaxExpressionCount int
}

// CacheConfig defines caching configuration
type CacheConfig struct {
	Enabled         bool
	TTL             time.Duration
	MaxSize         int
	CleanupInterval time.Duration
}

// CacheEntry represents a cached query result
type CacheEntry struct {
	Data      interface{}
	ExpiresAt time.Time
	CreatedAt time.Time
	HitCount  int64
}

// QueryCache interface for different cache implementations
type QueryCache interface {
	Get(key string) (interface{}, bool)
	Set(key string, value interface{}, ttl time.Duration)
	Delete(key string)
	Clear()
	Stats() CacheStats
}

// CacheStats provides cache performance metrics
type CacheStats struct {
	Hits        int64
	Misses      int64
	Size        int
	HitRate     float64
	MemoryUsage int64
}

// BatchOperation represents a single operation in a batch
type BatchOperation struct {
	ID      string
	Query   Figo
	Context any
	Type    string
}

// BatchResult represents the result of a batch operation
type BatchResult struct {
	ID      string
	Result  interface{}
	Error   error
	Success bool
}

// BatchProcessor handles batch operations
type BatchProcessor interface {
	Process(operations []BatchOperation) []BatchResult
	ProcessAsync(operations []BatchOperation) <-chan BatchResult
}

// InMemoryBatchProcessor implements BatchProcessor using in-memory processing
type InMemoryBatchProcessor struct {
	maxConcurrency int
	timeout        time.Duration
}

// Metrics represents performance metrics
type Metrics struct {
	QueryCount     int64
	CacheHits      int64
	CacheMisses    int64
	AverageLatency time.Duration
	TotalLatency   time.Duration
	ErrorCount     int64
	LastQueryTime  time.Time
}

// PerformanceMonitor tracks query performance
type PerformanceMonitor struct {
	metrics *Metrics
	enabled bool
	mu      sync.RWMutex
}

// NewPerformanceMonitor creates a new performance monitor
func NewPerformanceMonitor(enabled bool) *PerformanceMonitor {
	return &PerformanceMonitor{
		metrics: &Metrics{},
		enabled: enabled,
	}
}

// RecordQuery records a query execution
func (pm *PerformanceMonitor) RecordQuery(latency time.Duration, cacheHit bool, err error) {
	if !pm.enabled {
		return
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.metrics.QueryCount++
	pm.metrics.TotalLatency += latency
	pm.metrics.AverageLatency = time.Duration(int64(pm.metrics.TotalLatency) / pm.metrics.QueryCount)
	pm.metrics.LastQueryTime = time.Now()

	if cacheHit {
		pm.metrics.CacheHits++
	} else {
		pm.metrics.CacheMisses++
	}

	if err != nil {
		pm.metrics.ErrorCount++
	}
}

// GetMetrics returns current metrics
func (pm *PerformanceMonitor) GetMetrics() Metrics {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	// Create a copy to avoid returning a value that contains a lock
	return Metrics{
		QueryCount:     pm.metrics.QueryCount,
		CacheHits:      pm.metrics.CacheHits,
		CacheMisses:    pm.metrics.CacheMisses,
		AverageLatency: pm.metrics.AverageLatency,
		TotalLatency:   pm.metrics.TotalLatency,
		ErrorCount:     pm.metrics.ErrorCount,
		LastQueryTime:  pm.metrics.LastQueryTime,
	}
}

// Reset resets all metrics
func (pm *PerformanceMonitor) Reset() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.metrics = &Metrics{}
}

// Plugin System

// Plugin interface for extending Figo functionality
type Plugin interface {
	Name() string
	Version() string
	Initialize(f Figo) error
	BeforeQuery(f Figo, ctx any) error
	AfterQuery(f Figo, ctx any, result interface{}) error
	BeforeParse(f Figo, dsl string) (string, error)
	AfterParse(f Figo, dsl string) error
}

// PluginManager manages plugins
type PluginManager struct {
	plugins map[string]Plugin
	mu      sync.RWMutex
}

// NewPluginManager creates a new plugin manager
func NewPluginManager() *PluginManager {
	return &PluginManager{
		plugins: make(map[string]Plugin),
	}
}

// RegisterPlugin registers a plugin
func (pm *PluginManager) RegisterPlugin(plugin Plugin) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if plugin == nil {
		return fmt.Errorf("plugin cannot be nil")
	}

	name := plugin.Name()
	if name == "" {
		return fmt.Errorf("plugin name cannot be empty")
	}

	if _, exists := pm.plugins[name]; exists {
		return fmt.Errorf("plugin %s already registered", name)
	}

	pm.plugins[name] = plugin
	return nil
}

// UnregisterPlugin removes a plugin
func (pm *PluginManager) UnregisterPlugin(name string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if _, exists := pm.plugins[name]; !exists {
		return fmt.Errorf("plugin %s not found", name)
	}

	delete(pm.plugins, name)
	return nil
}

// GetPlugin retrieves a plugin by name
func (pm *PluginManager) GetPlugin(name string) (Plugin, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	plugin, exists := pm.plugins[name]
	return plugin, exists
}

// ListPlugins returns all registered plugins
func (pm *PluginManager) ListPlugins() []Plugin {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	plugins := make([]Plugin, 0, len(pm.plugins))
	for _, plugin := range pm.plugins {
		plugins = append(plugins, plugin)
	}
	return plugins
}

// ExecuteBeforeQuery executes all plugins' BeforeQuery hooks
func (pm *PluginManager) ExecuteBeforeQuery(f Figo, ctx any) error {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	for _, plugin := range pm.plugins {
		if err := plugin.BeforeQuery(f, ctx); err != nil {
			return fmt.Errorf("plugin %s BeforeQuery error: %w", plugin.Name(), err)
		}
	}
	return nil
}

// ExecuteAfterQuery executes all plugins' AfterQuery hooks
func (pm *PluginManager) ExecuteAfterQuery(f Figo, ctx any, result interface{}) error {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	for _, plugin := range pm.plugins {
		if err := plugin.AfterQuery(f, ctx, result); err != nil {
			return fmt.Errorf("plugin %s AfterQuery error: %w", plugin.Name(), err)
		}
	}
	return nil
}

// ExecuteBeforeParse executes all plugins' BeforeParse hooks
func (pm *PluginManager) ExecuteBeforeParse(f Figo, dsl string) (string, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	modifiedDSL := dsl
	for _, plugin := range pm.plugins {
		var err error
		modifiedDSL, err = plugin.BeforeParse(f, modifiedDSL)
		if err != nil {
			return "", fmt.Errorf("plugin %s BeforeParse error: %w", plugin.Name(), err)
		}
	}
	return modifiedDSL, nil
}

// ExecuteAfterParse executes all plugins' AfterParse hooks
func (pm *PluginManager) ExecuteAfterParse(f Figo, dsl string) error {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	for _, plugin := range pm.plugins {
		if err := plugin.AfterParse(f, dsl); err != nil {
			return fmt.Errorf("plugin %s AfterParse error: %w", plugin.Name(), err)
		}
	}
	return nil
}

// NewInMemoryBatchProcessor creates a new batch processor
func NewInMemoryBatchProcessor(maxConcurrency int, timeout time.Duration) *InMemoryBatchProcessor {
	return &InMemoryBatchProcessor{
		maxConcurrency: maxConcurrency,
		timeout:        timeout,
	}
}

// Process executes batch operations synchronously
func (bp *InMemoryBatchProcessor) Process(operations []BatchOperation) []BatchResult {
	results := make([]BatchResult, len(operations))

	// Use a semaphore to limit concurrency
	semaphore := make(chan struct{}, bp.maxConcurrency)
	var wg sync.WaitGroup

	for i, op := range operations {
		wg.Add(1)
		go func(index int, operation BatchOperation) {
			defer wg.Done()

			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// Execute operation with timeout
			result := bp.executeOperation(operation)
			results[index] = result
		}(i, op)
	}

	wg.Wait()
	return results
}

// ProcessAsync executes batch operations asynchronously
func (bp *InMemoryBatchProcessor) ProcessAsync(operations []BatchOperation) <-chan BatchResult {
	resultChan := make(chan BatchResult, len(operations))

	go func() {
		defer close(resultChan)

		// Use a semaphore to limit concurrency
		semaphore := make(chan struct{}, bp.maxConcurrency)
		var wg sync.WaitGroup

		for _, op := range operations {
			wg.Add(1)
			go func(operation BatchOperation) {
				defer wg.Done()

				// Acquire semaphore
				semaphore <- struct{}{}
				defer func() { <-semaphore }()

				// Execute operation with timeout
				result := bp.executeOperation(operation)
				resultChan <- result
			}(op)
		}

		wg.Wait()
	}()

	return resultChan
}

// executeOperation executes a single operation
func (bp *InMemoryBatchProcessor) executeOperation(operation BatchOperation) BatchResult {
	ctx, cancel := context.WithTimeout(context.Background(), bp.timeout)
	defer cancel()

	result := BatchResult{
		ID:      operation.ID,
		Success: false,
	}

	// Execute based on operation type
	switch operation.Type {
	case "sql":
		sql := operation.Query.GetSqlString(operation.Context)
		result.Result = sql
		result.Success = true
	case "query":
		query := operation.Query.GetQuery(operation.Context)
		result.Result = query
		result.Success = true
	case "cached_sql":
		sql := operation.Query.GetCachedSqlString(operation.Context)
		result.Result = sql
		result.Success = true
	case "cached_query":
		query := operation.Query.GetCachedQuery(operation.Context)
		result.Result = query
		result.Success = true
	default:
		result.Error = fmt.Errorf("unknown operation type: %s", operation.Type)
	}

	// Check for timeout
	select {
	case <-ctx.Done():
		result.Error = ctx.Err()
		result.Success = false
	default:
	}

	return result
}

// InMemoryCache implements QueryCache using in-memory storage
type InMemoryCache struct {
	mu       sync.RWMutex
	entries  map[string]*CacheEntry
	config   CacheConfig
	hits     int64
	misses   int64
	stopChan chan struct{}
}

// NewInMemoryCache creates a new in-memory cache instance
func NewInMemoryCache(config CacheConfig) *InMemoryCache {
	cache := &InMemoryCache{
		entries:  make(map[string]*CacheEntry),
		config:   config,
		stopChan: make(chan struct{}),
	}

	if config.Enabled && config.CleanupInterval > 0 {
		go cache.cleanup()
	}

	return cache
}

// Close properly stops the cache and cleans up resources
func (c *InMemoryCache) Close() {
	c.Stop()
}

// Get retrieves a value from the cache
func (c *InMemoryCache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, exists := c.entries[key]
	if !exists {
		c.misses++
		return nil, false
	}

	// Check if expired
	if time.Now().After(entry.ExpiresAt) {
		c.misses++
		return nil, false
	}

	entry.HitCount++
	c.hits++
	return entry.Data, true
}

// Set stores a value in the cache
func (c *InMemoryCache) Set(key string, value interface{}, ttl time.Duration) {
	if !c.config.Enabled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Check size limit
	if len(c.entries) >= c.config.MaxSize {
		c.evictLRU()
	}

	c.entries[key] = &CacheEntry{
		Data:      value,
		ExpiresAt: time.Now().Add(ttl),
		CreatedAt: time.Now(),
		HitCount:  0,
	}
}

// Delete removes a value from the cache
func (c *InMemoryCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Clear removes all entries from the cache
func (c *InMemoryCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*CacheEntry)
}

// Stats returns cache performance statistics
func (c *InMemoryCache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	total := c.hits + c.misses
	hitRate := float64(0)
	if total > 0 {
		hitRate = float64(c.hits) / float64(total)
	}

	return CacheStats{
		Hits:        c.hits,
		Misses:      c.misses,
		Size:        len(c.entries),
		HitRate:     hitRate,
		MemoryUsage: int64(len(c.entries) * 100), // Rough estimate
	}
}

// evictLRU removes the least recently used entry
func (c *InMemoryCache) evictLRU() {
	var oldestKey string
	var oldestTime time.Time

	for key, entry := range c.entries {
		if oldestKey == "" || entry.CreatedAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.CreatedAt
		}
	}

	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// cleanup removes expired entries periodically
func (c *InMemoryCache) cleanup() {
	ticker := time.NewTicker(c.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.cleanupExpired()
		case <-c.stopChan:
			return
		}
	}
}

// cleanupExpired removes expired entries
func (c *InMemoryCache) cleanupExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for key, entry := range c.entries {
		if now.After(entry.ExpiresAt) {
			delete(c.entries, key)
		}
	}
}

// Stop stops the cleanup goroutine
func (c *InMemoryCache) Stop() {
	close(c.stopChan)
}

// ParseError represents a DSL parsing error with context
type ParseError struct {
	Message    string
	Position   int
	Line       int
	Column     int
	Context    string
	Suggestion string
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("Parse error at line %d, column %d: %s", e.Line, e.Column, e.Message)
	}
	return fmt.Sprintf("Parse error at position %d: %s", e.Position, e.Message)
}

// Expr represents an ORM-agnostic expression node
type Expr interface{ isExpr() }

// Comparison expressions
type EqExpr struct {
	Field string
	Value any
}
type GteExpr struct {
	Field string
	Value any
}
type GtExpr struct {
	Field string
	Value any
}
type LtExpr struct {
	Field string
	Value any
}
type LteExpr struct {
	Field string
	Value any
}
type NeqExpr struct {
	Field string
	Value any
}
type LikeExpr struct {
	Field string
	Value any
}

// Regex expression
type RegexExpr struct {
	Field string
	Value any
}

// Logical expressions
type AndExpr struct{ Operands []Expr }
type OrExpr struct{ Operands []Expr }
type NotExpr struct{ Operands []Expr }

// Sorting expressions
type OrderByColumn struct {
	Name string
	Desc bool
}

type OrderBy struct{ Columns []OrderByColumn }

// Query is a marker interface for adapter-agnostic rendered queries
// Concrete types are provided per adapter (e.g., SQLQuery, MongoFindQuery)
type Query interface{ isQuery() }

// SQLQuery represents a parametrized SQL statement
type SQLQuery struct {
	SQL  string
	Args []any
}

func (SQLQuery) isQuery() {}

func (EqExpr) isExpr()    {}
func (GteExpr) isExpr()   {}
func (GtExpr) isExpr()    {}
func (LtExpr) isExpr()    {}
func (LteExpr) isExpr()   {}
func (NeqExpr) isExpr()   {}
func (LikeExpr) isExpr()  {}
func (RegexExpr) isExpr() {}
func (AndExpr) isExpr()   {}
func (OrExpr) isExpr()    {}
func (NotExpr) isExpr()   {}
func (OrderBy) isExpr()   {}

type InExpr struct {
	Field  string
	Values []any
}

type NotInExpr struct {
	Field  string
	Values []any
}

type BetweenExpr struct {
	Field string
	Low   any
	High  any
}

type IsNullExpr struct{ Field string }

type NotNullExpr struct{ Field string }

type ILikeExpr struct {
	Field string
	Value any
}

func (InExpr) isExpr()      {}
func (NotInExpr) isExpr()   {}
func (BetweenExpr) isExpr() {}
func (IsNullExpr) isExpr()  {}
func (NotNullExpr) isExpr() {}
func (ILikeExpr) isExpr()   {}

// Advanced Operators for Phase 3

// JsonPathExpr represents JSON path operations
type JsonPathExpr struct {
	Field string
	Path  string
	Value any
	Op    string // "=", "!=", ">", "<", ">=", "<=", "contains", "exists"
}

// ArrayContainsExpr represents array contains operations
type ArrayContainsExpr struct {
	Field  string
	Values []any
}

// ArrayOverlapsExpr represents array overlap operations
type ArrayOverlapsExpr struct {
	Field  string
	Values []any
}

// FullTextSearchExpr represents full-text search operations
type FullTextSearchExpr struct {
	Field    string
	Query    string
	Language string // Optional language for full-text search
}

// GeoDistanceExpr represents geographical distance operations
type GeoDistanceExpr struct {
	Field     string
	Latitude  float64
	Longitude float64
	Distance  float64 // in kilometers
	Unit      string  // "km", "miles", "meters"
}

// CustomExpr represents custom operations
type CustomExpr struct {
	Field    string
	Operator string
	Value    any
	Handler  func(field, operator string, value any) (string, []any, error)
}

// Validation System

// ValidationRule represents a validation rule
type ValidationRule struct {
	Field   string
	Rule    string
	Value   any
	Message string
	Handler func(field, rule string, value any) error
}

// Validator interface for custom validation
type Validator interface {
	Validate(field, rule string, value any) error
	GetRuleName() string
}

// ValidationManager manages validation rules
type ValidationManager struct {
	rules      []ValidationRule
	validators map[string]Validator
	mu         sync.RWMutex
}

// NewValidationManager creates a new validation manager
func NewValidationManager() *ValidationManager {
	return &ValidationManager{
		rules:      make([]ValidationRule, 0),
		validators: make(map[string]Validator),
	}
}

// AddRule adds a validation rule
func (vm *ValidationManager) AddRule(rule ValidationRule) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.rules = append(vm.rules, rule)
}

// RegisterValidator registers a custom validator
func (vm *ValidationManager) RegisterValidator(validator Validator) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.validators[validator.GetRuleName()] = validator
}

// Validate validates a field value against all applicable rules
func (vm *ValidationManager) Validate(field string, value any) error {
	vm.mu.RLock()
	defer vm.mu.RUnlock()

	for _, rule := range vm.rules {
		if rule.Field == field || rule.Field == "*" {
			if rule.Handler != nil {
				if err := rule.Handler(field, rule.Rule, value); err != nil {
					return fmt.Errorf("validation failed for field %s: %s", field, rule.Message)
				}
			} else if validator, exists := vm.validators[rule.Rule]; exists {
				if err := validator.Validate(field, rule.Rule, value); err != nil {
					return fmt.Errorf("validation failed for field %s: %s", field, rule.Message)
				}
			}
		}
	}
	return nil
}

// Built-in validators
type RequiredValidator struct{}

func (v RequiredValidator) Validate(field, rule string, value any) error {
	if value == nil || value == "" {
		return fmt.Errorf("field %s is required", field)
	}
	return nil
}
func (v RequiredValidator) GetRuleName() string { return "required" }

type MinLengthValidator struct{}

func (v MinLengthValidator) Validate(field, rule string, value any) error {
	if str, ok := value.(string); ok {
		if len(str) < 3 { // Example minimum length
			return fmt.Errorf("field %s must be at least 3 characters", field)
		}
	}
	return nil
}
func (v MinLengthValidator) GetRuleName() string { return "min_length" }

type EmailValidator struct{}

func (v EmailValidator) Validate(field, rule string, value any) error {
	if str, ok := value.(string); ok {
		if !strings.Contains(str, "@") {
			return fmt.Errorf("field %s must be a valid email", field)
		}
	}
	return nil
}
func (v EmailValidator) GetRuleName() string { return "email" }

// Implement Expr interface for new operators
func (JsonPathExpr) isExpr()       {}
func (ArrayContainsExpr) isExpr()  {}
func (ArrayOverlapsExpr) isExpr()  {}
func (FullTextSearchExpr) isExpr() {}
func (GeoDistanceExpr) isExpr()    {}
func (CustomExpr) isExpr()         {}

type Figo interface {
	AddFiltersFromString(input string) error
	AddFiltersFromStringWithRepair(input string, useRepair bool) error
	AddFilter(exp Expr)
	AddIgnoreFields(fields ...string)
	AddSelectFields(fields ...string)
	SetAllowedFields(fields ...string)
	EnableFieldWhitelist()
	DisableFieldWhitelist()
	IsFieldAllowed(field string) bool
	SetQueryLimits(limits QueryLimits)
	GetQueryLimits() QueryLimits
	ParseFieldsValue(str string) any
	IsFieldWhitelistEnabled() bool
	GetDSL() string
	SetCache(cache QueryCache)
	GetCache() QueryCache
	SetCacheConfig(config CacheConfig)
	GetCacheConfig() CacheConfig
	GetCacheStats() CacheStats
	ClearCache()
	SetPerformanceMonitor(monitor *PerformanceMonitor)
	GetPerformanceMonitor() *PerformanceMonitor
	GetMetrics() Metrics
	ResetMetrics()
	SetPluginManager(manager *PluginManager)
	GetPluginManager() *PluginManager
	RegisterPlugin(plugin Plugin) error
	UnregisterPlugin(name string) error
	SetValidationManager(manager *ValidationManager)
	GetValidationManager() *ValidationManager
	AddValidationRule(rule ValidationRule)
	RegisterValidator(validator Validator)
	ValidateField(field string, value any) error
	SetNamingStrategy(strategy NamingStrategy)
	SetPage(skip, take int)
	SetPageString(v string)
	SetAdapterObject(adapter Adapter)
	GetNamingStrategy() NamingStrategy
	GetIgnoreFields() map[string]bool
	GetSelectFields() map[string]bool
	GetAllowedFields() map[string]bool
	GetClauses() []Expr
	GetPreloads() map[string][]Expr
	GetPage() Page
	GetSort() *OrderBy
	GetAdapterObject() Adapter
	GetSqlString(ctx any, conditionType ...string) string
	GetExplainedSqlString(ctx any, conditionType ...string) string
	GetQuery(ctx any, conditionType ...string) Query
	GetCachedSqlString(ctx any, conditionType ...string) string
	GetCachedQuery(ctx any, conditionType ...string) Query
	Build()
}

type Adapter interface {
	GetSqlString(f Figo, ctx any, conditionType ...string) (string, bool)
	GetQuery(f Figo, ctx any, conditionType ...string) (Query, bool)
}

type figo struct {
	clauses           []Expr
	preloads          map[string][]Expr
	page              Page
	sort              *OrderBy
	ignoreFields      map[string]bool
	selectFields      map[string]bool
	allowedFields     map[string]bool
	fieldWhitelist    bool
	queryLimits       QueryLimits
	cache             QueryCache
	cacheConfig       CacheConfig
	monitor           *PerformanceMonitor
	pluginManager     *PluginManager
	validationManager *ValidationManager
	dsl               string
	namingStrategy    NamingStrategy
	adapterObj        Adapter
	mu                sync.RWMutex // Mutex for concurrent access protection
}

// Constructor: use New(adapter) with an Adapter object (or nil)

// New constructs a new instance with the specified adapter object. Pass nil for no adapter.
func New(adapter Adapter) Figo {
	f := &figo{page: Page{
		Skip: 0,
		Take: 20,
	}, preloads: make(map[string][]Expr), ignoreFields: make(map[string]bool), selectFields: make(map[string]bool), allowedFields: make(map[string]bool), fieldWhitelist: false, queryLimits: QueryLimits{
		MaxNestingDepth:    10,
		MaxFieldCount:      50,
		MaxParameterCount:  100,
		MaxExpressionCount: 200,
	}, clauses: make([]Expr, 0), namingStrategy: NAMING_STRATEGY_SNAKE_CASE}
	f.adapterObj = adapter
	return f
}

func (p *Page) validate() {
	if p.Skip < 0 {
		p.Skip = 0
	}
	if p.Take < 0 {
		p.Take = 0
	}
}

type Node struct {
	Expression []Expr
	Operator   Operation
	Value      string
	Field      string
	Children   []*Node
	Parent     *Node
}

// validateParentheses checks if parentheses are properly matched
func (f *figo) validateParentheses(expr string) bool {
	count := 0
	for _, char := range expr {
		if char == '(' {
			count++
		} else if char == ')' {
			count--
			if count < 0 {
				return false // Unmatched closing parenthesis
			}
		}
	}
	return count == 0 // All parentheses matched
}

// validateParenthesesWithPosition checks parentheses with detailed error reporting
func (f *figo) validateParenthesesWithPosition(expr string) error {
	count := 0
	line := 1
	column := 1
	var lastOpenPos int

	for i, char := range expr {
		if char == '\n' {
			line++
			column = 1
		} else {
			column++
		}

		if char == '(' {
			count++
			lastOpenPos = i
		} else if char == ')' {
			count--
			if count < 0 {
				return &ParseError{
					Message:    "unmatched closing parenthesis",
					Position:   i,
					Line:       line,
					Column:     column,
					Context:    expr,
					Suggestion: "Remove extra closing parenthesis or add opening parenthesis",
				}
			}
		}
	}

	if count > 0 {
		return &ParseError{
			Message:    "unmatched opening parenthesis",
			Position:   lastOpenPos,
			Line:       line,
			Column:     column,
			Context:    expr,
			Suggestion: "Add closing parenthesis to match opening one",
		}
	}

	return nil
}

// validateQuotes checks if quotes are properly matched
func (f *figo) validateQuotes(expr string) bool {
	inQuotes := false
	quoteChar := rune(0)

	for _, char := range expr {
		if char == '"' || char == '\'' {
			if !inQuotes {
				inQuotes = true
				quoteChar = char
			} else if char == quoteChar {
				inQuotes = false
				quoteChar = 0
			}
		}
	}

	return !inQuotes // All quotes properly closed
}

// validateQuotesWithPosition checks quotes with detailed error reporting
func (f *figo) validateQuotesWithPosition(expr string) error {
	inQuotes := false
	quoteChar := rune(0)
	line := 1
	column := 1
	var quoteStartPos int

	for i, char := range expr {
		if char == '\n' {
			line++
			column = 1
		} else {
			column++
		}

		if char == '"' || char == '\'' {
			if !inQuotes {
				inQuotes = true
				quoteChar = char
				quoteStartPos = i
			} else if char == quoteChar {
				inQuotes = false
				quoteChar = 0
			}
		}
	}

	if inQuotes {
		return &ParseError{
			Message:    "unmatched quote",
			Position:   quoteStartPos,
			Line:       line,
			Column:     column,
			Context:    expr,
			Suggestion: "Add closing quote to match opening one",
		}
	}

	return nil
}

// validateBrackets checks if brackets are properly matched for load expressions
func (f *figo) validateBrackets(expr string) error {
	count := 0
	line := 1
	column := 1
	var lastOpenPos int

	for i, char := range expr {
		if char == '\n' {
			line++
			column = 1
		} else {
			column++
		}

		if char == '[' {
			count++
			lastOpenPos = i
		} else if char == ']' {
			count--
			if count < 0 {
				return &ParseError{
					Message:    "unmatched closing bracket",
					Position:   i,
					Line:       line,
					Column:     column,
					Context:    expr,
					Suggestion: "Remove extra closing bracket or add opening bracket",
				}
			}
		}
	}

	if count > 0 {
		return &ParseError{
			Message:    "unmatched opening bracket",
			Position:   lastOpenPos,
			Line:       line,
			Column:     column,
			Context:    expr,
			Suggestion: "Add closing bracket to match opening one",
		}
	}

	return nil
}

// validateBasicSyntax checks for common syntax errors
func (f *figo) validateBasicSyntax(expr string) error {
	// Check for common malformed patterns
	patterns := []struct {
		pattern    string
		message    string
		suggestion string
	}{
		{`\s+and\s*$`, "incomplete AND expression", "Add field and value after AND"},
		{`\s+or\s*$`, "incomplete OR expression", "Add field and value after OR"},
		{`\s+not\s*$`, "incomplete NOT expression", "Add expression after NOT"},
		{`=\s*$`, "incomplete equality expression", "Add value after ="},
		{`>\s*$`, "incomplete greater than expression", "Add value after >"},
		{`<\s*$`, "incomplete less than expression", "Add value after <"},
		{`!=\s*$`, "incomplete not equal expression", "Add value after !="},
		{`>=\s*$`, "incomplete greater than or equal expression", "Add value after >="},
		{`<=\s*$`, "incomplete less than or equal expression", "Add value after <="},
		{`=^\s*$`, "incomplete LIKE expression", "Add value after =^"},
		{`!=^\s*$`, "incomplete NOT LIKE expression", "Add value after !=^"},
		{`=~\s*$`, "incomplete regex expression", "Add value after =~"},
		{`!=~\s*$`, "incomplete NOT regex expression", "Add value after !=~"},
		{`<in>\s*$`, "incomplete IN expression", "Add value list after <in>"},
		{`<nin>\s*$`, "incomplete NOT IN expression", "Add value list after <nin>"},
		{`<bet>\s*$`, "incomplete BETWEEN expression", "Add value range after <bet>"},
		{`^\s*and\b`, "expression starts with AND", "Remove AND or add field before it"},
		{`^\s*or\b`, "expression starts with OR", "Remove OR or add field before it"},
		{`^\s*not\b`, "expression starts with NOT", "Add expression after NOT"},
	}

	for _, p := range patterns {
		if matched, _ := regexp.MatchString(p.pattern, expr); matched {
			return &ParseError{
				Message:    p.message,
				Position:   0,
				Line:       1,
				Column:     1,
				Context:    expr,
				Suggestion: p.suggestion,
			}
		}
	}

	return nil
}

func (f *figo) parseDSL(expr string) *Node {
	root := &Node{Value: "root", Expression: make([]Expr, 0)}
	stack := []*Node{root}
	current := root
outerLoop:
	for i := 0; i < len(expr); {
		switch expr[i] {
		case '(':
			newNode := &Node{Operator: "----", Parent: current}
			current.Children = append(current.Children, newNode)
			stack = append(stack, newNode)
			current = newNode
			i++
		case ')':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
				current = stack[len(stack)-1]
			}
			i++
		case ' ':
			i++
		default:
			j := i
			ff := -1
			for j < len(expr) {

				if expr[j] == '"' && ff == -1 {
					ff = 1
					j++
					continue
				}
				if expr[j] == '"' && ff == 1 {
					ff = 0
					j++
					break

				}

				if expr[j] != '"' && ff == 1 {
					j++
					continue
				}

				if expr[j] == ' ' && ff == -1 {
					break
				}

				if expr[j] == ' ' && ff == 0 {
					break
				}

				j++
			}
			token := strings.TrimSpace(expr[i:j])
			if token != "" {
				// Check if this is a logical operator (not, and, or)
				if token == "not" || token == "and" || token == "or" {
					// Handle logical operators
					var op Operation
					switch token {
					case "not":
						op = OperationNot
					case "and":
						op = OperationAnd
					case "or":
						op = OperationOr
					}

					// Create a node for the logical operator
					newNode := &Node{Operator: op, Value: token, Field: "", Parent: current, Expression: make([]Expr, 0)}
					current.Children = append(current.Children, newNode)
					i = j
					continue
				}

				if strings.HasPrefix(token, string(OperationSort)) || strings.HasPrefix(token, string(OperationPage)) || strings.HasPrefix(token, string(OperationLoad)) {
					k := j - 1
					if strings.HasPrefix(token, string(OperationLoad)) {
						bracketCount := 1
						for k < len(expr) && bracketCount > 0 {

							switch expr[k] {
							case '[':
								bracketCount++
							case ']':
								bracketCount--
							}
							k++

						}
						//k++

						loadLabel := fmt.Sprintf("%v=[", string(OperationLoad))

						v := strings.TrimSpace(expr[i:k])
						labelIndex := strings.Index(v, loadLabel)
						if labelIndex == -1 {
							i = k
							continue
						}
						content := v[labelIndex+len(loadLabel) : len(v)-1]
						if content == "" {
							i = k
							continue
						}

						loadSplit := strings.Split(content, "|")
						for _, l := range loadSplit {
							colonIndex := strings.Index(l, ":")
							if colonIndex == -1 {
								continue
							}
							rawTable := l[:colonIndex]
							table := strings.TrimSpace(rawTable)
							loadContent := strings.TrimSpace(l[colonIndex+1:])

							loadRootNode := f.parseDSL(loadContent)
							expressionParser(loadRootNode)
							loadExpr := getFinalExpr(*loadRootNode)
							if loadExpr != nil {
								f.preloads[table] = append(f.preloads[table], loadExpr)
							}

						}
						i = k
						continue

					} else if strings.HasPrefix(token, string(OperationPage)) {

						pageLabel := fmt.Sprintf("%v=", string(OperationPage))
						content := token[strings.Index(token, pageLabel)+len(pageLabel):]

						pageContent := strings.Split(content, ",")

						for _, s := range pageContent {
							pageSplit := strings.Split(s, ":")
							if len(pageSplit) != 2 {
								continue
							}

							field := pageSplit[0]
							value := pageSplit[1]

							parseInt, parsErr := strconv.ParseInt(value, 10, 64)
							if parsErr == nil {

								switch field {
								case "skip":
									f.page.Skip = int(parseInt)
								case "take":
									f.page.Take = int(parseInt)
								}

								f.page.validate()
							}

						}

					} else if strings.HasPrefix(token, string(OperationSort)) {

						sortLabel := fmt.Sprintf("%v=", string(OperationSort))
						content := token[strings.Index(token, sortLabel)+len(sortLabel):]

						sortContent := strings.Split(content, ",")

						var c []OrderByColumn

						for _, s := range sortContent {
							sortSplit := strings.Split(s, ":")
							if len(sortSplit) != 2 {
								continue
							}

							field := sortSplit[0]
							value := sortSplit[1]

							c = append(c, OrderByColumn{
								Name: f.parsFieldsName(field),
								Desc: strings.ToLower(value) == "desc",
							})

						}

						sortExpr := OrderBy{
							Columns: c,
						}
						f.sort = &sortExpr

					} else {
						for k < len(expr) && expr[k] != ' ' && expr[k] != '(' && expr[k] != ')' {
							k++
						}
					}

					i = k
				} else {
					// Try to combine tokens for expressions like "field > value" or "field =^ value"
					// Only do this for very specific cases to avoid interfering with complex operators
					combinedToken := token
					// Only combine if the token looks like a simple field name (alphanumeric + underscores)
					// and doesn't contain any operators or special characters
					if isSimpleFieldName(token) {
						// This looks like a field name with underscores, try to combine with next tokens
						nextStart := j
						for nextStart < len(expr) && expr[nextStart] == ' ' {
							nextStart++
						}
						if nextStart < len(expr) {
							nextEnd := nextStart
							nextFF := -1
							for nextEnd < len(expr) {
								if expr[nextEnd] == '"' && nextFF == -1 {
									nextFF = 1
									nextEnd++
									continue
								}
								if expr[nextEnd] == '"' && nextFF == 1 {
									nextFF = 0
									nextEnd++
									break
								}
								if expr[nextEnd] != '"' && nextFF == 1 {
									nextEnd++
									continue
								}
								if expr[nextEnd] == ' ' && nextFF == -1 {
									break
								}
								if expr[nextEnd] == ' ' && nextFF == 0 {
									break
								}
								nextEnd++
							}
							nextToken := strings.TrimSpace(expr[nextStart:nextEnd])
							// Combine with both simple and complex operators
							if nextToken == ">" || nextToken == "<" || nextToken == "=" || nextToken == "!=" || nextToken == ">=" || nextToken == "<=" || nextToken == "=^" || nextToken == "!=^" || nextToken == ".=^" || nextToken == "=~" || nextToken == "!=~" || nextToken == "<in>" || nextToken == "<nin>" || nextToken == "<bet>" || nextToken == "<null>" || nextToken == "<notnull>" {
								combinedToken = token + " " + nextToken
								j = nextEnd

								// Try to get the value token as well
								if nextToken == "<bet>" || nextToken == "<in>" || nextToken == "<nin>" {
									valueStart := j
									for valueStart < len(expr) && expr[valueStart] == ' ' {
										valueStart++
									}
									if valueStart < len(expr) {
										valueEnd := valueStart
										valueFF := -1
										parenCount := 0
										for valueEnd < len(expr) {
											if expr[valueEnd] == '"' && valueFF == -1 {
												valueFF = 1
												valueEnd++
												continue
											}
											if expr[valueEnd] == '"' && valueFF == 1 {
												valueFF = 0
												valueEnd++
												break
											}
											if expr[valueEnd] != '"' && valueFF == 1 {
												valueEnd++
												continue
											}
											// Handle parentheses for BETWEEN operations
											if expr[valueEnd] == '(' && valueFF == -1 {
												parenCount++
											}
											if expr[valueEnd] == ')' && valueFF == -1 {
												parenCount--
												if parenCount == 0 {
													valueEnd++
													break
												}
											}
											// Stop at spaces, parentheses, or logical operators (but not inside parentheses)
											if (expr[valueEnd] == ' ' || expr[valueEnd] == ')' || expr[valueEnd] == '(') && valueFF == -1 && parenCount == 0 {
												break
											}
											if expr[valueEnd] == ' ' && valueFF == 0 && parenCount == 0 {
												break
											}
											valueEnd++
										}
										valueToken := strings.TrimSpace(expr[valueStart:valueEnd])
										if valueToken != "" && !strings.Contains(valueToken, "=") && !strings.Contains(valueToken, ">") && !strings.Contains(valueToken, "<") && !strings.Contains(valueToken, "!") && valueToken != "and" && valueToken != "or" && valueToken != "not" && !strings.Contains(valueToken, "page=") && !strings.Contains(valueToken, "sort=") && !strings.Contains(valueToken, "load=") {
											combinedToken = combinedToken + " " + valueToken
											j = valueEnd
										}
									}
								} else {
									// For simple operators, use simpler value extraction
									valueStart := j
									for valueStart < len(expr) && expr[valueStart] == ' ' {
										valueStart++
									}
									if valueStart < len(expr) {
										valueEnd := valueStart
										valueFF := -1
										for valueEnd < len(expr) {
											if expr[valueEnd] == '"' && valueFF == -1 {
												valueFF = 1
												valueEnd++
												continue
											}
											if expr[valueEnd] == '"' && valueFF == 1 {
												valueFF = 0
												valueEnd++
												break
											}
											if expr[valueEnd] != '"' && valueFF == 1 {
												valueEnd++
												continue
											}
											// Stop at spaces, parentheses, or logical operators
											if (expr[valueEnd] == ' ' || expr[valueEnd] == ')' || expr[valueEnd] == '(') && valueFF == -1 {
												break
											}
											if expr[valueEnd] == ' ' && valueFF == 0 {
												break
											}
											valueEnd++
										}
										valueToken := strings.TrimSpace(expr[valueStart:valueEnd])
										if valueToken != "" && !strings.Contains(valueToken, "=") && !strings.Contains(valueToken, ">") && !strings.Contains(valueToken, "<") && !strings.Contains(valueToken, "!") && valueToken != "and" && valueToken != "or" && valueToken != "not" && !strings.Contains(valueToken, "page=") && !strings.Contains(valueToken, "sort=") && !strings.Contains(valueToken, "load=") {
											combinedToken = combinedToken + " " + valueToken
											j = valueEnd
										}
									}
								}
							}
						}
					}

					operator, valueStr, field := parseToken(combinedToken)
					value := f.parsFieldsValue(valueStr)

					// Check if this is a logical operator
					valueStrForOp := fmt.Sprintf("%v", value)
					if operator == "" && Operation(valueStrForOp) != OperationAnd && Operation(valueStrForOp) != OperationOr && Operation(valueStrForOp) != OperationNot {
						i = j
						continue
					} else {

						// Check ignore fields
						for ignoreField := range f.ignoreFields {
							if field == ignoreField {
								i = j
								continue outerLoop
							}
						}

						// Field whitelist check will be done during expression building

					}

					// Convert field name after whitelist check
					convertedField := f.parsFieldsName(field)
					newNode := &Node{Operator: operator, Value: valueStrForOp, Field: convertedField, Parent: current, Expression: make([]Expr, 0)}
					if Operation(valueStrForOp) == OperationAnd || Operation(valueStrForOp) == OperationOr || Operation(valueStrForOp) == OperationNot {
						newNode.Operator = Operation(valueStrForOp)
					} else {

						newNode.Expression = append(newNode.Expression, getClausesFromOperation(operator, convertedField, value))
					}
					current.Children = append(current.Children, newNode)
					i = j
				}
			} else {
				i = j
			}
		}
	}

	return root
}

func (f *figo) parsFieldsName(str string) string {
	switch f.namingStrategy {
	case NAMING_STRATEGY_NO_CHANGE:
		return str
	case NAMING_STRATEGY_SNAKE_CASE:
		// Use stringy to convert to snake_case, but handle edge cases
		result := stringy.New(str).SnakeCase("?", "").ToLower()
		// If stringy returns empty string, fallback to original string
		if result == "" {
			return str
		}
		return result
	default:
		return ""
	}
}

func (f *figo) parsFieldsValue(str string) any {
	s := strings.TrimSpace(str)

	// Handle quoted strings - remove quotes but keep as string
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		quotedStr := s[1 : len(s)-1] // Remove quotes

		// Try to parse quoted string as date
		if dateVal, err := parseDate(quotedStr); err == nil {
			return dateVal
		}

		return quotedStr
	}

	// Parse boolean values (only for unquoted values)
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}

	// Parse null values
	if s == "null" || s == "NULL" {
		return nil
	}

	// Parse numeric values (only for unquoted values)
	if s != "" {
		// Try to parse as integer
		if intVal, err := strconv.ParseInt(s, 10, 64); err == nil {
			return intVal
		}
		// Try to parse as float
		if floatVal, err := strconv.ParseFloat(s, 64); err == nil {
			return floatVal
		}

		// Try to parse as date (unquoted)
		if dateVal, err := parseDate(s); err == nil {
			return dateVal
		}
	}

	// Return as string
	return s
}

// parseDate attempts to parse a string as a date using common formats
func parseDate(s string) (time.Time, error) {
	// Common date formats to try
	formats := []string{
		time.RFC3339,           // 2006-01-02T15:04:05Z07:00
		time.RFC3339Nano,       // 2006-01-02T15:04:05.999999999Z07:00
		"2006-01-02T15:04:05Z", // 2006-01-02T15:04:05Z
		"2006-01-02T15:04:05",  // 2006-01-02T15:04:05
		"2006-01-02 15:04:05",  // 2006-01-02 15:04:05
		"2006-01-02",           // 2006-01-02
		"2006/01/02",           // 2006/01/02
		"01/02/2006",           // 01/02/2006 (US format)
		"02/01/2006",           // 02/01/2006 (EU format)
		"Jan 2, 2006",          // Jan 2, 2006
		"January 2, 2006",      // January 2, 2006
	}

	for _, format := range formats {
		if t, err := time.Parse(format, s); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse date: %s", s)
}

// isSimpleFieldName checks if a token looks like a simple field name
func isSimpleFieldName(token string) bool {
	// Must not be empty
	if token == "" {
		return false
	}

	// Must not contain operators or special characters
	if strings.Contains(token, "=") || strings.Contains(token, ">") || strings.Contains(token, "<") ||
		strings.Contains(token, "!") || strings.Contains(token, "^") || strings.Contains(token, "~") ||
		strings.Contains(token, "page=") || strings.Contains(token, "sort=") || strings.Contains(token, "load=") {
		return false
	}

	// Must not be logical operators
	if token == "and" || token == "or" || token == "not" {
		return false
	}

	// Must not be complex operators
	if token == "like" || token == "in" || token == "between" || token == "null" || token == "notnull" {
		return false
	}

	// Must contain only alphanumeric characters and underscores
	for _, char := range token {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_') {
			return false
		}
	}

	return true
}

func parseToken(token string) (Operation, string, string) {
	// Order matters: place custom multi-char markers first
	operators := []Operation{
		OperationNotRegex,
		OperationRegex,
		OperationNotLike,
		OperationILike,
		OperationNotIn,
		OperationIn,
		OperationBetween,
		OperationNotNull,
		OperationIsNull,
		OperationLike,
		OperationGte, OperationLte,
		OperationNeq, OperationGt, OperationLt, OperationEq,
	}
	for _, op := range operators {
		if strings.Contains(token, string(op)) {
			parts := strings.Split(token, string(op))
			var right string
			if len(parts) > 1 {
				right = parts[1]
			}
			field := strings.TrimSpace(parts[0])
			return op, right, field
		}
	}
	return "", token, ""
}

func getClausesFromOperation(o Operation, field string, value any) Expr {
	// helper to parse a single scalar literal: preserve quoted strings, parse unquoted numerics
	parseScalarValue := func(raw string) any {
		s := strings.TrimSpace(raw)
		if len(s) >= 2 && strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
			return strings.Trim(s, "\"")
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
		if f64, err := strconv.ParseFloat(s, 64); err == nil {
			return f64
		}
		return s
	}

	// helper to parse a list literal like [1,2,"x"]
	parseListValue := func(raw string) []any {
		s := strings.TrimSpace(raw)
		if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
			s = strings.TrimPrefix(s, "[")
			s = strings.TrimSuffix(s, "]")
		}
		if s == "" {
			return nil
		}
		parts := strings.Split(s, ",")
		vals := make([]any, 0, len(parts))
		for _, p := range parts {
			vals = append(vals, parseScalarValue(p))
		}
		return vals
	}

	// helper to parse a string literal for LIKE operations (always string)
	parseLikeValue := func(raw string) string {
		s := strings.TrimSpace(raw)
		if len(s) >= 2 && strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
			return strings.Trim(s, "\"")
		}
		return s
	}

	switch o {
	case OperationEq:
		return EqExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationGte:
		return GteExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationGt:
		return GtExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationLt:
		return LtExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationLte:
		return LteExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationNeq:
		return NeqExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationLike:
		return LikeExpr{Field: field, Value: parseLikeValue(fmt.Sprintf("%v", value))}
	case OperationNotLike:
		return NotExpr{Operands: []Expr{LikeExpr{Field: field, Value: parseLikeValue(fmt.Sprintf("%v", value))}}}
	case OperationRegex:
		return RegexExpr{Field: field, Value: parseLikeValue(fmt.Sprintf("%v", value))}
	case OperationNotRegex:
		return NotExpr{Operands: []Expr{RegexExpr{Field: field, Value: parseLikeValue(fmt.Sprintf("%v", value))}}}
	case OperationIn:
		vals := parseListValue(fmt.Sprintf("%v", value))
		return InExpr{Field: field, Values: vals}
	case OperationNotIn:
		vals := parseListValue(fmt.Sprintf("%v", value))
		return NotInExpr{Field: field, Values: vals}
	case OperationBetween:
		s := strings.TrimSpace(fmt.Sprintf("%v", value))
		// strip optional parentheses
		if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
			s = strings.TrimPrefix(s, "(")
			s = strings.TrimSuffix(s, ")")
		}
		if idx := strings.Index(s, ".."); idx > 0 {
			low := strings.TrimSpace(s[:idx])
			high := strings.TrimSpace(s[idx+2:])
			return BetweenExpr{Field: field, Low: parseScalarValue(low), High: parseScalarValue(high)}
		}
		return nil
	case OperationILike:
		return ILikeExpr{Field: field, Value: parseLikeValue(fmt.Sprintf("%v", value))}
	case OperationIsNull:
		return IsNullExpr{Field: field}
	case OperationNotNull:
		return NotNullExpr{Field: field}
	default:
		return nil
	}
}

func expressionParser(node *Node) {
	// First, recursively process all child nodes to build their expressions
	for _, child := range node.Children {
		if child.Operator == OperationChild {
			expressionParser(child)
		}
	}

	if node.Operator == OperationChild {
		if len(node.Children) == 1 {
			node.Expression = append(node.Expression, node.Children[0].Expression...)
			return
		}

		// For multiple children, build proper logical expression tree with precedence
		expr := buildExpressionTreeWithPrecedence(node.Children)
		if expr != nil {
			node.Expression = append(node.Expression, expr)
		}
	} else {
		// For non-child nodes, recursively process children
		for _, child := range node.Children {
			if child.Operator == OperationChild {
				expressionParser(child)
			}
		}

		// After processing children, build expression tree with precedence
		expr := buildExpressionTreeWithPrecedence(node.Children)
		if expr != nil {
			node.Expression = append(node.Expression, expr)
		}
	}
}

// buildExpressionTree builds a simple expression tree
func buildExpressionTree(children []*Node) Expr {
	if len(children) == 0 {
		return nil
	}

	// If we have only one child, return its expression
	if len(children) == 1 {
		child := children[0]
		if len(child.Expression) > 0 {
			return child.Expression[len(child.Expression)-1]
		}
		return nil
	}

	// For multiple children, we need to build a proper logical expression tree
	// The expressionParser should have already built the logical expressions
	// We just need to find the final expression that represents the entire tree

	// Look for the most recent logical expression that combines all operands
	for i := len(children) - 1; i >= 0; i-- {
		child := children[i]

		// Skip child nodes (parentheses groups) as they should be processed separately
		if child.Operator == OperationChild {
			continue
		}

		// Look for logical operators that have expressions
		if (child.Operator == OperationAnd || child.Operator == OperationOr || child.Operator == OperationNot) &&
			len(child.Expression) > 0 {
			// Return the most recent expression from this logical operator
			return child.Expression[len(child.Expression)-1]
		}
	}

	// If no logical expressions found, return nil
	return nil
}

// buildExpressionTreeWithPrecedence builds a proper expression tree respecting operator precedence
func buildExpressionTreeWithPrecedence(children []*Node) Expr {
	if len(children) == 0 {
		return nil
	}

	// Build a list of expressions and operators in order
	var items []interface{} // Can be Expr or Operation

	for _, child := range children {
		// Add expressions from this child
		if len(child.Expression) > 0 {
			items = append(items, child.Expression[len(child.Expression)-1])
		}
		// Add operators
		if child.Operator == OperationAnd || child.Operator == OperationOr || child.Operator == OperationNot {
			items = append(items, child.Operator)
		}
	}

	if len(items) == 0 {
		return nil
	}

	// If we have only one item and it's an expression, return it
	if len(items) == 1 {
		if expr, ok := items[0].(Expr); ok {
			return expr
		}
		return nil
	}

	// Process with proper precedence: NOT > AND > OR
	return processWithPrecedence(items)
}

// processWithPrecedence processes expressions with proper operator precedence
func processWithPrecedence(items []interface{}) Expr {
	if len(items) == 0 {
		return nil
	}

	// Convert to a more manageable structure
	var expressions []Expr
	var operators []Operation

	for _, item := range items {
		switch v := item.(type) {
		case Expr:
			expressions = append(expressions, v)
		case Operation:
			operators = append(operators, v)
		}
	}

	if len(expressions) == 0 {
		return nil
	}

	// If we have only one expression, return it
	if len(expressions) == 1 {
		return expressions[0]
	}

	// Process operators in precedence order: NOT > AND > OR
	// We need to handle this more carefully to respect the tree structure

	// First pass: Handle NOT operators (highest precedence)
	// NOT operators should be applied to the next expression
	for i := 0; i < len(operators); i++ {
		if operators[i] == OperationNot {
			// Find the next expression to negate
			if i < len(expressions) {
				expressions[i] = NotExpr{Operands: []Expr{expressions[i]}}
				// Remove the NOT operator
				operators = append(operators[:i], operators[i+1:]...)
				i-- // Adjust index
			}
		}
	}

	// Second pass: Handle AND operators (medium precedence)
	// Process AND operators from left to right
	for i := 0; i < len(operators); i++ {
		if operators[i] == OperationAnd {
			// Find the expressions to combine
			left := i
			right := i + 1

			if left < len(expressions) && right < len(expressions) {
				// Create AND expression
				andExpr := AndExpr{Operands: []Expr{expressions[left], expressions[right]}}

				// Replace the two expressions with the combined one
				expressions = append(expressions[:left], append([]Expr{andExpr}, expressions[right+1:]...)...)

				// Remove the AND operator
				operators = append(operators[:i], operators[i+1:]...)
				i-- // Adjust index
			}
		}
	}

	// Third pass: Handle OR operators (lowest precedence)
	// Process OR operators from left to right
	for i := 0; i < len(operators); i++ {
		if operators[i] == OperationOr {
			// Find the expressions to combine
			left := i
			right := i + 1

			if left < len(expressions) && right < len(expressions) {
				// Create OR expression
				orExpr := OrExpr{Operands: []Expr{expressions[left], expressions[right]}}

				// Replace the two expressions with the combined one
				expressions = append(expressions[:left], append([]Expr{orExpr}, expressions[right+1:]...)...)

				// Remove the OR operator
				operators = append(operators[:i], operators[i+1:]...)
				i-- // Adjust index
			}
		}
	}

	// Return the final expression
	if len(expressions) > 0 {
		return expressions[0]
	}
	return nil
}

func getFinalExpr(node Node) Expr {
	// If the node itself has expressions, return the last one
	if len(node.Expression) > 0 {
		return node.Expression[len(node.Expression)-1]
	}

	// If no children, return nil
	if len(node.Children) == 0 {
		return nil
	}

	// If only one child, return its expression
	if len(node.Children) == 1 {
		child := node.Children[0]
		if len(child.Expression) > 0 {
			// Return the last (most recent) expression from the child
			return child.Expression[len(child.Expression)-1]
		}
		return nil
	}

	// For multiple children, we need to build a proper logical expression tree
	// The expressionParser should have already built the logical expressions
	// We just need to find the final expression that represents the entire tree

	// Look for the most recent logical expression that combines all operands
	for i := len(node.Children) - 1; i >= 0; i-- {
		child := node.Children[i]

		// Skip child nodes (parentheses groups) as they should be processed separately
		if child.Operator == OperationChild {
			continue
		}

		// Look for logical operators that have expressions
		if (child.Operator == OperationAnd || child.Operator == OperationOr || child.Operator == OperationNot) &&
			len(child.Expression) > 0 {
			// Return the most recent expression from this logical operator
			return child.Expression[len(child.Expression)-1]
		}
	}

	// If no logical expressions found, return nil
	return nil
}

// validateInput validates the input DSL string with enhanced error reporting
func (f *figo) validateInput(input string) error {
	// Validate parentheses with position tracking
	if err := f.validateParenthesesWithPosition(input); err != nil {
		return err
	}

	// Validate quotes with position tracking
	if err := f.validateQuotesWithPosition(input); err != nil {
		return err
	}

	// Validate brackets for load expressions
	if err := f.validateBrackets(input); err != nil {
		return err
	}

	// Validate basic syntax patterns
	if err := f.validateBasicSyntax(input); err != nil {
		return err
	}

	return nil
}

// attemptInputRepair tries to fix common malformed input patterns
func (f *figo) attemptInputRepair(input string) (string, error) {
	original := input
	fixed := input

	// Fix common patterns - be more conservative
	repairs := []struct {
		pattern     *regexp.Regexp
		replacement string
		description string
	}{
		{regexp.MustCompile(`\s+and\s*$`), "", "Remove trailing AND"},
		{regexp.MustCompile(`\s+or\s*$`), "", "Remove trailing OR"},
		{regexp.MustCompile(`\s+not\s*$`), "", "Remove trailing NOT"},
		{regexp.MustCompile(`^\s*and\b`), "", "Remove leading AND"},
		{regexp.MustCompile(`^\s*or\b`), "", "Remove leading OR"},
		{regexp.MustCompile(`^\s*not\b`), "", "Remove leading NOT"},
	}

	// Apply repairs
	for _, repair := range repairs {
		if repair.pattern.MatchString(fixed) {
			fixed = repair.pattern.ReplaceAllString(fixed, repair.replacement)
		}
	}

	// Try to fix unmatched parentheses
	if !f.validateParentheses(fixed) {
		fixed = f.fixUnmatchedParentheses(fixed)
	}

	// Try to fix unmatched quotes
	if !f.validateQuotes(fixed) {
		fixed = f.fixUnmatchedQuotes(fixed)
	}

	// Try to fix unmatched brackets
	if err := f.validateBrackets(fixed); err != nil {
		fixed = f.fixUnmatchedBrackets(fixed)
	}

	// If no changes were made, return original
	if fixed == original {
		return original, fmt.Errorf("no repairs could be applied")
	}

	// Validate the fixed input
	if err := f.validateInput(fixed); err != nil {
		return original, fmt.Errorf("repair failed validation: %w", err)
	}

	return fixed, nil
}

// fixUnmatchedParentheses attempts to fix unmatched parentheses
func (f *figo) fixUnmatchedParentheses(input string) string {
	count := 0
	result := strings.Builder{}

	for _, char := range input {
		if char == '(' {
			count++
			result.WriteRune(char)
		} else if char == ')' {
			if count > 0 {
				count--
				result.WriteRune(char)
			}
			// Skip extra closing parentheses
		} else {
			result.WriteRune(char)
		}
	}

	// Add missing closing parentheses
	for i := 0; i < count; i++ {
		result.WriteRune(')')
	}

	return result.String()
}

// fixUnmatchedQuotes attempts to fix unmatched quotes
func (f *figo) fixUnmatchedQuotes(input string) string {
	inQuotes := false
	quoteChar := rune(0)
	result := strings.Builder{}

	for _, char := range input {
		if char == '"' || char == '\'' {
			if !inQuotes {
				inQuotes = true
				quoteChar = char
				result.WriteRune(char)
			} else if char == quoteChar {
				inQuotes = false
				quoteChar = 0
				result.WriteRune(char)
			} else {
				result.WriteRune(char)
			}
		} else {
			result.WriteRune(char)
		}
	}

	// Add missing closing quote
	if inQuotes {
		result.WriteRune(quoteChar)
	}

	return result.String()
}

// fixUnmatchedBrackets attempts to fix unmatched brackets
func (f *figo) fixUnmatchedBrackets(input string) string {
	count := 0
	result := strings.Builder{}

	for _, char := range input {
		if char == '[' {
			count++
			result.WriteRune(char)
		} else if char == ']' {
			if count > 0 {
				count--
				result.WriteRune(char)
			}
			// Skip extra closing brackets
		} else {
			result.WriteRune(char)
		}
	}

	// Add missing closing brackets
	for i := 0; i < count; i++ {
		result.WriteRune(']')
	}

	return result.String()
}

func (f *figo) AddFiltersFromString(input string) error {
	// Handle empty input
	if strings.TrimSpace(input) == "" {
		return nil
	}

	// Execute BeforeParse plugin hooks
	if f.pluginManager != nil {
		var err error
		input, err = f.pluginManager.ExecuteBeforeParse(f, input)
		if err != nil {
			return fmt.Errorf("plugin BeforeParse error: %w", err)
		}
	}

	// Update DSL string (replace existing) - protected by mutex
	f.mu.Lock()
	f.dsl = input
	f.mu.Unlock()

	// Execute AfterParse plugin hooks
	if f.pluginManager != nil {
		err := f.pluginManager.ExecuteAfterParse(f, input)
		if err != nil {
			return fmt.Errorf("plugin AfterParse error: %w", err)
		}
	}

	return nil
}

// AddFiltersFromStringWithRepair allows controlling whether input repair is used
func (f *figo) AddFiltersFromStringWithRepair(input string, useRepair bool) error {
	// Handle empty input
	if strings.TrimSpace(input) == "" {
		return nil
	}

	var fixedInput string
	var err error

	if useRepair {
		// Try to fix common malformed input patterns
		fixedInput, err = f.attemptInputRepair(input)
		if err != nil {
			// If repair fails, validate original input
			if validationErr := f.validateInput(input); validationErr != nil {
				return validationErr
			}
			fixedInput = input
		}
	} else {
		// Validate input without repair
		if err := f.validateInput(input); err != nil {
			return err
		}
		fixedInput = input
	}

	// Execute BeforeParse plugin hooks
	if f.pluginManager != nil {
		var err error
		fixedInput, err = f.pluginManager.ExecuteBeforeParse(f, fixedInput)
		if err != nil {
			return fmt.Errorf("plugin BeforeParse error: %w", err)
		}
	}

	// Update DSL string (replace existing) - protected by mutex
	f.mu.Lock()
	f.dsl = fixedInput
	f.mu.Unlock()

	// Execute AfterParse plugin hooks
	if f.pluginManager != nil {
		err := f.pluginManager.ExecuteAfterParse(f, fixedInput)
		if err != nil {
			return fmt.Errorf("plugin AfterParse error: %w", err)
		}
	}

	return nil
}

func (f *figo) AddFilter(exp Expr) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.clauses = append(f.clauses, exp)
}

func (f *figo) AddIgnoreFields(fields ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, field := range fields {
		f.ignoreFields[field] = true
	}
}

func (f *figo) AddSelectFields(fields ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, field := range fields {
		f.selectFields[field] = true
	}
}

func (f *figo) GetIgnoreFields() map[string]bool {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Return a copy to avoid race conditions
	result := make(map[string]bool)
	for k, v := range f.ignoreFields {
		result[k] = v
	}
	return result
}

func (f *figo) GetClauses() []Expr {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Return a copy to avoid race conditions
	result := make([]Expr, len(f.clauses))
	copy(result, f.clauses)
	return result
}

func (f *figo) GetPreloads() map[string][]Expr {

	return f.preloads
}

func (f *figo) GetPage() Page {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.page
}

func (f *figo) GetSort() *OrderBy {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.sort
}

func (f *figo) SetPage(skip, take int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.page.Skip = skip
	f.page.Take = take
	f.page.validate()
}

func (f *figo) SetPageString(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	pageContent := strings.Split(v, ",")

	for _, s := range pageContent {
		pageSplit := strings.Split(s, ":")
		if len(pageSplit) != 2 {
			continue
		}

		field := pageSplit[0]
		value := pageSplit[1]

		parseInt, parsErr := strconv.ParseInt(value, 10, 64)
		if parsErr == nil {
			switch field {
			case "skip":
				f.page.Skip = int(parseInt)
			case "take":
				f.page.Take = int(parseInt)
			}

			f.page.validate()
		}

	}
}

func (f *figo) GetSelectFields() map[string]bool {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Return a copy to avoid race conditions
	result := make(map[string]bool)
	for k, v := range f.selectFields {
		result[k] = v
	}
	return result
}

func (f *figo) SetNamingStrategy(strategy NamingStrategy) {
	f.namingStrategy = strategy
}

func (f *figo) GetNamingStrategy() NamingStrategy {

	return f.namingStrategy
}

func (f *figo) SetAdapterObject(adapter Adapter) {
	f.adapterObj = adapter
}

func (f *figo) GetAdapterObject() Adapter {
	return f.adapterObj
}

// GetSqlString returns a SQL string based on the selected adapter.
// For AdapterGorm, ctx should be a *gorm.DB configured with Model(...).
// For AdapterRaw, ctx can be a table name (string) or RawContext.
func (f *figo) GetSqlString(ctx any, conditionType ...string) string {
	if f.adapterObj != nil {
		if sql, ok := f.adapterObj.GetSqlString(f, ctx, conditionType...); ok {
			return sql
		}
		return ""
	}
	return ""
}

// GetExplainedSqlString returns a SQL string with placeholders expanded for easier debugging
func (f *figo) GetExplainedSqlString(ctx any, conditionType ...string) string {
	if f.adapterObj != nil {
		if sql, ok := f.adapterObj.GetSqlString(f, ctx, conditionType...); ok {
			return sql
		}
		return ""
	}
	return ""
}

// GetQuery delegates to the configured adapter to obtain a typed query object
func (f *figo) GetQuery(ctx any, conditionType ...string) Query {
	if f.adapterObj != nil {
		if q, ok := f.adapterObj.GetQuery(f, ctx, conditionType...); ok {
			return q
		}
		return nil
	}
	return nil
}

// Field Whitelisting Methods

// SetAllowedFields sets the list of allowed fields for querying
func (f *figo) SetAllowedFields(fields ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.allowedFields = make(map[string]bool)
	for _, field := range fields {
		f.allowedFields[field] = true
	}
}

// EnableFieldWhitelist enables field whitelist validation
func (f *figo) EnableFieldWhitelist() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fieldWhitelist = true
}

// DisableFieldWhitelist disables field whitelist validation
func (f *figo) DisableFieldWhitelist() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fieldWhitelist = false
}

// IsFieldAllowed checks if a field is allowed for querying
func (f *figo) IsFieldAllowed(field string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.isFieldAllowedUnsafe(field)
}

// isFieldAllowedUnsafe checks if a field is allowed without acquiring locks
// This should only be called when the caller already holds the appropriate lock
func (f *figo) isFieldAllowedUnsafe(field string) bool {
	if !f.fieldWhitelist {
		return true // If whitelist is disabled, all fields are allowed
	}
	return f.allowedFields[field]
}

// GetAllowedFields returns the map of allowed fields
func (f *figo) GetAllowedFields() map[string]bool {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Return a copy to avoid race conditions
	result := make(map[string]bool)
	for k, v := range f.allowedFields {
		result[k] = v
	}
	return result
}

// SetQueryLimits sets the query complexity limits
func (f *figo) SetQueryLimits(limits QueryLimits) {
	f.queryLimits = limits
}

// GetQueryLimits returns the current query limits
func (f *figo) GetQueryLimits() QueryLimits {
	return f.queryLimits
}

// ParseFieldsValue parses a string value with enhanced type support
func (f *figo) ParseFieldsValue(str string) any {
	return f.parsFieldsValue(str)
}

// IsFieldWhitelistEnabled returns whether field whitelist is enabled
func (f *figo) IsFieldWhitelistEnabled() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.fieldWhitelist
}

// GetDSL returns the current DSL string
func (f *figo) GetDSL() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.dsl
}

func (f *figo) Build() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.dsl == "" {
		return
	}

	// Clear existing clauses before rebuilding
	f.clauses = []Expr{}

	root := f.parseDSL(f.dsl)
	expressionParser(root)

	finalExpr := getFinalExpr(*root)

	if finalExpr != nil {
		// Apply field whitelist filtering if enabled
		if f.fieldWhitelist {
			filteredExpr := f.filterAllowedFields(finalExpr)
			if filteredExpr != nil {
				f.clauses = append(f.clauses, filteredExpr)
			}
		} else {
			f.clauses = append(f.clauses, finalExpr)
		}
	}
}

// filterAllowedFields recursively filters out disallowed fields from expressions
func (f *figo) filterAllowedFields(expr Expr) Expr {
	switch e := expr.(type) {
	case EqExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case GteExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case GtExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case LtExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case LteExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case NeqExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case LikeExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case ILikeExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case RegexExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case InExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case NotInExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case BetweenExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case IsNullExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case NotNullExpr:
		if f.isFieldAllowedUnsafe(e.Field) {
			return e
		}
		return nil
	case AndExpr:
		var filteredOperands []Expr
		for _, operand := range e.Operands {
			if filtered := f.filterAllowedFields(operand); filtered != nil {
				filteredOperands = append(filteredOperands, filtered)
			}
		}
		if len(filteredOperands) == 0 {
			return nil
		}
		if len(filteredOperands) == 1 {
			return filteredOperands[0]
		}
		return AndExpr{Operands: filteredOperands}
	case OrExpr:
		var filteredOperands []Expr
		for _, operand := range e.Operands {
			if filtered := f.filterAllowedFields(operand); filtered != nil {
				filteredOperands = append(filteredOperands, filtered)
			}
		}
		if len(filteredOperands) == 0 {
			return nil
		}
		if len(filteredOperands) == 1 {
			return filteredOperands[0]
		}
		return OrExpr{Operands: filteredOperands}
	case NotExpr:
		var filteredOperands []Expr
		for _, operand := range e.Operands {
			if filtered := f.filterAllowedFields(operand); filtered != nil {
				filteredOperands = append(filteredOperands, filtered)
			}
		}
		if len(filteredOperands) == 0 {
			return nil
		}
		return NotExpr{Operands: filteredOperands}
	default:
		return e
	}
}

// Cache Management Methods

// SetCache sets the query cache instance
func (f *figo) SetCache(cache QueryCache) {
	f.cache = cache
}

// GetCache returns the current cache instance
func (f *figo) GetCache() QueryCache {
	return f.cache
}

// SetCacheConfig sets the cache configuration
func (f *figo) SetCacheConfig(config CacheConfig) {
	f.cacheConfig = config
	if f.cache == nil && config.Enabled {
		f.cache = NewInMemoryCache(config)
	}
}

// GetCacheConfig returns the current cache configuration
func (f *figo) GetCacheConfig() CacheConfig {
	return f.cacheConfig
}

// GetCacheStats returns cache performance statistics
func (f *figo) GetCacheStats() CacheStats {
	if f.cache == nil {
		return CacheStats{}
	}
	return f.cache.Stats()
}

// ClearCache clears all cached entries
func (f *figo) ClearCache() {
	if f.cache != nil {
		f.cache.Clear()
	}
}

// generateCacheKey creates a unique cache key for a query
func (f *figo) generateCacheKey(ctx any, conditionType ...string) string {
	// Create a hash of the query components
	components := []string{
		f.dsl,
		fmt.Sprintf("%v", f.clauses),
		fmt.Sprintf("%v", f.preloads),
		fmt.Sprintf("%v", f.page),
		fmt.Sprintf("%v", f.sort),
		fmt.Sprintf("%v", f.ignoreFields),
		fmt.Sprintf("%v", f.selectFields),
		fmt.Sprintf("%v", f.allowedFields),
		fmt.Sprintf("%v", f.fieldWhitelist),
		fmt.Sprintf("%v", f.namingStrategy),
		fmt.Sprintf("%v", ctx),
		fmt.Sprintf("%v", conditionType),
	}

	content := strings.Join(components, "|")
	hash := md5.Sum([]byte(content))
	return fmt.Sprintf("figo:%x", hash)
}

// GetCachedSqlString retrieves SQL string from cache or generates it
func (f *figo) GetCachedSqlString(ctx any, conditionType ...string) string {
	start := time.Now()
	var cacheHit bool
	var err error

	defer func() {
		latency := time.Since(start)
		f.recordQueryExecution(latency, cacheHit, err)
	}()

	if f.cache == nil || !f.cacheConfig.Enabled {
		return f.GetSqlString(ctx, conditionType...)
	}

	key := f.generateCacheKey(ctx, conditionType...)

	// Try to get from cache
	if cached, found := f.cache.Get(key); found {
		if sql, ok := cached.(string); ok {
			cacheHit = true
			return sql
		}
	}

	// Generate and cache
	sql := f.GetSqlString(ctx, conditionType...)
	f.cache.Set(key, sql, f.cacheConfig.TTL)
	cacheHit = false

	return sql
}

// GetCachedQuery retrieves query from cache or generates it
func (f *figo) GetCachedQuery(ctx any, conditionType ...string) Query {
	start := time.Now()
	var cacheHit bool
	var err error

	defer func() {
		latency := time.Since(start)
		f.recordQueryExecution(latency, cacheHit, err)
	}()

	if f.cache == nil || !f.cacheConfig.Enabled {
		return f.GetQuery(ctx, conditionType...)
	}

	key := f.generateCacheKey(ctx, conditionType...)

	// Try to get from cache
	if cached, found := f.cache.Get(key); found {
		if query, ok := cached.(Query); ok {
			cacheHit = true
			return query
		}
	}

	// Generate and cache
	query := f.GetQuery(ctx, conditionType...)
	if query != nil {
		f.cache.Set(key, query, f.cacheConfig.TTL)
	}
	cacheHit = false

	return query
}

// Performance Monitoring Methods

// SetPerformanceMonitor sets the performance monitor
func (f *figo) SetPerformanceMonitor(monitor *PerformanceMonitor) {
	f.monitor = monitor
}

// GetPerformanceMonitor returns the current performance monitor
func (f *figo) GetPerformanceMonitor() *PerformanceMonitor {
	return f.monitor
}

// GetMetrics returns current performance metrics
func (f *figo) GetMetrics() Metrics {
	if f.monitor == nil {
		return Metrics{}
	}
	return f.monitor.GetMetrics()
}

// ResetMetrics resets all performance metrics
func (f *figo) ResetMetrics() {
	if f.monitor != nil {
		f.monitor.Reset()
	}
}

// recordQueryExecution records a query execution for monitoring
func (f *figo) recordQueryExecution(latency time.Duration, cacheHit bool, err error) {
	if f.monitor != nil {
		f.monitor.RecordQuery(latency, cacheHit, err)
	}
}

// Plugin Management Methods

// SetPluginManager sets the plugin manager
func (f *figo) SetPluginManager(manager *PluginManager) {
	f.pluginManager = manager
}

// GetPluginManager returns the current plugin manager
func (f *figo) GetPluginManager() *PluginManager {
	return f.pluginManager
}

// RegisterPlugin registers a plugin
func (f *figo) RegisterPlugin(plugin Plugin) error {
	if f.pluginManager == nil {
		f.pluginManager = NewPluginManager()
	}

	if err := f.pluginManager.RegisterPlugin(plugin); err != nil {
		return err
	}

	// Initialize the plugin
	return plugin.Initialize(f)
}

// UnregisterPlugin removes a plugin
func (f *figo) UnregisterPlugin(name string) error {
	if f.pluginManager == nil {
		return fmt.Errorf("no plugin manager available")
	}
	return f.pluginManager.UnregisterPlugin(name)
}

// Validation Management Methods

// SetValidationManager sets the validation manager
func (f *figo) SetValidationManager(manager *ValidationManager) {
	f.validationManager = manager
}

// GetValidationManager returns the current validation manager
func (f *figo) GetValidationManager() *ValidationManager {
	return f.validationManager
}

// AddValidationRule adds a validation rule
func (f *figo) AddValidationRule(rule ValidationRule) {
	if f.validationManager == nil {
		f.validationManager = NewValidationManager()
	}
	f.validationManager.AddRule(rule)
}

// RegisterValidator registers a custom validator
func (f *figo) RegisterValidator(validator Validator) {
	if f.validationManager == nil {
		f.validationManager = NewValidationManager()
	}
	f.validationManager.RegisterValidator(validator)
}

// ValidateField validates a field value
func (f *figo) ValidateField(field string, value any) error {
	if f.validationManager == nil {
		return nil // No validation if no manager
	}
	return f.validationManager.Validate(field, value)
}
