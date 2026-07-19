# 🔌 Figo Plugin System - Complete Guide

## Overview

The Figo Plugin System allows you to extend and customize the query builder's functionality through a powerful hook-based architecture. Plugins can intercept and modify queries at various stages of the processing pipeline.

## Plugin Interface

```go
type Plugin interface {
    Name() string
    Version() string
    Initialize(f Figo) error
    BeforeQuery(f Figo, ctx any) error
    AfterQuery(f Figo, ctx any, result interface{}) error
    BeforeParse(f Figo, dsl string) (string, error)
    AfterParse(f Figo, dsl string) error
}
```

## Plugin Lifecycle Hooks

### 1. Initialize Hook
- **When**: Called when the plugin is registered
- **Purpose**: Set up plugin resources, validate configuration
- **Return**: Error if initialization fails

### 2. BeforeParse Hook
- **When**: Called before DSL string is parsed
- **Purpose**: Modify the DSL string before parsing
- **Return**: Modified DSL string and error
- **Example**: Transform field names, add default conditions

### 3. AfterParse Hook
- **When**: Called after DSL string is parsed
- **Purpose**: Post-process the parsed expressions
- **Return**: Error if processing fails. An error also rolls the instance's DSL back to its previous value, so a rejected DSL can never be built later by a caller that ignored the error.
- **Example**: Validate parsed expressions, add metadata

### 4. BeforeQuery Hook
- **When**: Called automatically before every `GetSqlString` / `GetQuery` render (on cached paths: on misses, when a render actually happens)
- **Purpose**: Authorization checks, auditing, pre-render logic
- **Return**: Error to veto the render — `GetSqlString` returns `""`, `GetQuery` returns `nil`
- **Example**: Block rendering for unauthorized contexts, log query attempts
- **Note**: Must not render through the same instance (that would recurse)

### 5. AfterQuery Hook
- **When**: Called automatically after a successful render, with the result (the SQL `string` or the `Query`)
- **Purpose**: Observe or veto rendered output
- **Return**: Error to veto the result — the caller gets `""` / `nil`
- **Example**: Log rendered SQL, reject queries matching a deny-pattern

### 6. FilterExpr Hook (optional interface)

`ExprFilter` is an *optional* interface — implement it on your plugin alongside the base `Plugin` methods and figo picks it up automatically by type assertion:

```go
type ExprFilter interface {
    FilterExpr(f Figo, e Expr) Expr // return nil to drop the expression
}
```

- **When**: Called by `Build` on the parsed expression tree (and every preload condition) and by `AddFilter` on programmatic expressions
- **Purpose**: Transform or prune expressions as they enter the clause tree
- **Return**: The (possibly rewritten) expression, or `nil` to drop it entirely
- **Example**: The built-in `FieldsPlugin` uses this for ignore-list and whitelist pruning
- **Note**: Runs outside the instance's lock, so calling back into `f`'s read methods is safe

### 7. FinalizeClauses Hook (optional interface)

```go
type ClauseFinalizer interface {
    FinalizeClauses(f Figo, clauses []Expr) []Expr
}
```

- **When**: Called once at the end of **every** `Build` — including a Build whose DSL produced no filters (the list may be empty)
- **Purpose**: Transform the finished top-level clause list; the returned slice replaces the instance's clauses
- **Example**: The built-in `ScopePlugin` uses this to guarantee mandatory filters (tenant scoping) are always present
- **Note**: Runs after all `ExprFilter` passes, outside the instance's lock

## Creating a Plugin

### Basic Plugin Structure

```go
type MyPlugin struct {
    name    string
    version string
    enabled bool
}

func NewMyPlugin() *MyPlugin {
    return &MyPlugin{
        name:    "my-plugin",
        version: "1.0.0",
        enabled: true,
    }
}

func (p *MyPlugin) Name() string { return p.name }
func (p *MyPlugin) Version() string { return p.version }

func (p *MyPlugin) Initialize(f Figo) error {
    fmt.Printf("Plugin '%s' initialized\n", p.name)
    return nil
}

func (p *MyPlugin) BeforeQuery(f Figo, ctx any) error {
    // Pre-query logic
    return nil
}

func (p *MyPlugin) AfterQuery(f Figo, ctx any, result interface{}) error {
    // Post-query logic
    return nil
}

func (p *MyPlugin) BeforeParse(f Figo, dsl string) (string, error) {
    // Transform DSL before parsing
    return dsl, nil
}

func (p *MyPlugin) AfterParse(f Figo, dsl string) error {
    // Post-parse logic
    return nil
}
```

## Example: Field Name Transformer Plugin

Here's a complete example that transforms "id" to "idd" before parsing:

```go
package main

import (
    "fmt"
    "strings"
    "github.com/bi0dread/figo/v4"
)

type IdToIddPlugin struct {
    name    string
    version string
    enabled bool
}

func NewIdToIddPlugin() *IdToIddPlugin {
    return &IdToIddPlugin{
        name:    "id-to-idd-plugin",
        version: "1.0.0",
        enabled: true,
    }
}

func (p *IdToIddPlugin) Name() string { return p.name }
func (p *IdToIddPlugin) Version() string { return p.version }

func (p *IdToIddPlugin) Initialize(f figo.Figo) error {
    fmt.Printf("🔌 Plugin '%s' v%s initialized\n", p.name, p.version)
    return nil
}

func (p *IdToIddPlugin) BeforeQuery(f figo.Figo, ctx any) error {
    if !p.enabled { return nil }
    fmt.Printf("🔍 BeforeQuery: Plugin '%s' processing query\n", p.name)
    return nil
}

func (p *IdToIddPlugin) AfterQuery(f figo.Figo, ctx any, result interface{}) error {
    if !p.enabled { return nil }
    fmt.Printf("✅ AfterQuery: Plugin '%s' completed query processing\n", p.name)
    return nil
}

func (p *IdToIddPlugin) BeforeParse(f figo.Figo, dsl string) (string, error) {
    if !p.enabled { return dsl, nil }
    
    fmt.Printf("🔄 BeforeParse: Original DSL: %s\n", dsl)
    
    // Transform "id" to "idd"
    modifiedDSL := p.replaceIdWithIdd(dsl)
    
    fmt.Printf("🔄 BeforeParse: Modified DSL: %s\n", modifiedDSL)
    
    return modifiedDSL, nil
}

func (p *IdToIddPlugin) AfterParse(f figo.Figo, dsl string) error {
    if !p.enabled { return nil }
    fmt.Printf("🎯 AfterParse: Plugin '%s' completed parsing\n", p.name)
    return nil
}

func (p *IdToIddPlugin) replaceIdWithIdd(dsl string) string {
    // Handle various operator combinations
    dsl = strings.ReplaceAll(dsl, "id=", "idd=")
    dsl = strings.ReplaceAll(dsl, "id!=", "idd!=")
    dsl = strings.ReplaceAll(dsl, "id>", "idd>")
    dsl = strings.ReplaceAll(dsl, "id<", "idd<")
    dsl = strings.ReplaceAll(dsl, "id>=", "idd>=")
    dsl = strings.ReplaceAll(dsl, "id<=", "idd<=")
    
    // Handle space-separated operators
    dsl = strings.ReplaceAll(dsl, "id like", "idd like")
    dsl = strings.ReplaceAll(dsl, "id in", "idd in")
    dsl = strings.ReplaceAll(dsl, "id between", "idd between")
    dsl = strings.ReplaceAll(dsl, "id isnull", "idd isnull")
    dsl = strings.ReplaceAll(dsl, "id notnull", "idd notnull")
    
    // Handle parentheses
    dsl = strings.ReplaceAll(dsl, "(id ", "(idd ")
    dsl = strings.ReplaceAll(dsl, " id)", " idd)")
    
    // Handle standalone "id"
    words := strings.Fields(dsl)
    for i, word := range words {
        if word == "id" {
            words[i] = "idd"
        }
    }
    
    return strings.Join(words, " ")
}

func (p *IdToIddPlugin) Enable() { p.enabled = true }
func (p *IdToIddPlugin) Disable() { p.enabled = false }
func (p *IdToIddPlugin) IsEnabled() bool { return p.enabled }
```

## Built-in Plugins

Figo ships eight plugins out of the box: `ValidationPlugin`, `CachePlugin`, `MetricsPlugin`, `FieldsPlugin`, `LimitsPlugin`, `SyntaxPlugin`, `ScopePlugin`, and `AuditPlugin`.

### Scoping (mandatory filters)

`ScopePlugin` guarantees filters are present in every built query — the multi-tenant classic. It uses the `FinalizeClauses` hook, which runs at the end of **every** Build (even one with no filters), so an unfiltered query cannot escape the scope. Injection happens after `FieldsPlugin` pruning, so a whitelist never strips the scope:

```go
sp := plugins.NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: tenantID})
f.RegisterPlugin(sp)

f.AddFiltersFromString(untrustedDSL)
f.Build(adapters.RawAdapter{})
// WHERE (...caller filters...) AND `tenant_id` = ?
```

### Auditing

`AuditPlugin` records every parsed DSL (`AfterParse`) and every rendered statement (`AfterQuery` — real renders only, not cache hits) into an optional `slog.Logger` and a bounded in-memory history:

```go
ap := plugins.NewAuditPlugin(slog.Default(), 100) // nil logger = history only; 0 size = log only
f.RegisterPlugin(ap)

for _, e := range ap.History() {
	fmt.Println(e.Kind, e.DSL, e.Result) // "parse"/"query" entries, oldest first
}
```

### Validation

`ValidationPlugin` attaches validation rules to fields and enforces them through the `AfterParse` hook — once registered, any `AddFiltersFromString` call whose filter values violate a rule fails with an error.

```go
vp := plugins.NewValidationPlugin()
vp.RegisterValidator(plugins.EmailValidator{})   // built-ins: required, min_length, email
vp.AddRule(plugins.ValidationRule{Field: "email", Rule: "email", Message: "invalid email"})

f := figo.New()
f.RegisterPlugin(vp)

err := f.AddFiltersFromString(`email="not-an-email"`) // validation error
err = vp.Validate("email", "x@y.com")                 // direct one-off check
```

Custom rules can supply a `Handler` func instead of a named validator, and custom validators implement the `Validator` interface (`Validate`, `GetRuleName`).

### Caching

`CachePlugin` caches rendered SQL/query results keyed by the full state of the instance passed to it. Unlike the validation plugin it doesn't rely on hooks — you render *through* it, so registration is optional (register it if you want it discoverable via the plugin manager). One plugin can serve many `Figo` instances.

```go
cp := plugins.NewCachePlugin(plugins.CacheConfig{
	Enabled: true,
	TTL:     time.Minute,
	MaxSize: 1000,
})
defer cp.Close()

sql := cp.GetCachedSqlString(f, adapters.RawContext{Table: "users"})
q := cp.GetCachedQuery(f, adapters.RawContext{Table: "users"})

stats := cp.Stats()
cp.Clear()
```

Cache hits and misses are reported into the plugin's attached `PerformanceMonitor` when one is set (`cp.SetPerformanceMonitor(...)` — see Metrics below). Inject a custom store with `cp.SetCache(myQueryCache)`.

### Metrics

`MetricsPlugin` wraps a `PerformanceMonitor` as a registerable plugin. Attach its embedded monitor to whatever produces metrics — typically the cache plugin:

```go
mp := plugins.NewMetricsPlugin(true)
cp.SetPerformanceMonitor(mp.PerformanceMonitor)

m := mp.GetMetrics() // QueryCount, CacheHits, CacheMisses, AverageLatency, ...
mp.Reset()
```

You can also record manually from your own code via `mp.RecordQuery(latency, cacheHit, err)`.

### Field policy

`FieldsPlugin` carries the ignore list and the allowed-fields whitelist. It implements the `FilterExpr` hook, so once registered its pruning applies to every expression entering the clause tree — parsed DSL, preload conditions, and programmatic `AddFilter` calls alike:

```go
fp := plugins.NewFieldsPlugin()
fp.AddIgnoreFields("internal_flag")
fp.SetAllowedFields("id", "name", "email")
fp.EnableFieldWhitelist()
f.RegisterPlugin(fp)

f.AddFiltersFromString(`id=1 and internal_flag=true and secret="x"`)
f.Build(adapters.RawAdapter{}) // only id=1 survives
```

Ignore names match both raw and naming-converted spellings. Select fields (`AddSelectFields`) are not part of this plugin — they are projection state on the instance itself.

### Syntax validation & repair

`SyntaxPlugin` validates DSL syntax through `BeforeParse` — balanced parentheses, quotes, and brackets, plus common malformed patterns — failing `AddFiltersFromString` with a structured `*ParseError` (line/column/suggestion). With repair enabled it first fixes what it safely can (a leading `not` is never stripped):

```go
f.RegisterPlugin(plugins.NewSyntaxPlugin(false)) // strict
f.RegisterPlugin(plugins.NewSyntaxPlugin(true))  // repair, then validate
```

### Query limits

`LimitsPlugin` enforces complexity limits on parsed DSL via `AfterParse` — a real guard for untrusted input. A zero value disables that particular limit:

```go
lp := plugins.NewLimitsPlugin(plugins.DefaultQueryLimits())
f.RegisterPlugin(lp)

err := f.AddFiltersFromString(deepUntrustedDSL)
// "plugin figo-limits AfterParse error: query exceeds MaxNestingDepth: 12 > 10"
```

## Using Plugins

### 1. Register a Plugin

```go
// Create Figo instance
f := figo.New()

// Create and register plugin
plugin := NewIdToIddPlugin()
err := f.RegisterPlugin(plugin)
if err != nil {
    log.Fatal("Failed to register plugin:", err)
}
```

### 2. Plugin Manager

```go
// Get plugin manager
manager := f.GetPluginManager()

// List all plugins
plugins := manager.ListPlugins()
for _, plugin := range plugins {
    fmt.Printf("Plugin: %s v%s\n", plugin.Name(), plugin.Version())
}

// Get specific plugin
plugin, exists := manager.GetPlugin("my-plugin")
if exists {
    // Use plugin
}
```

### 3. Manual Hook Execution

All hooks fire automatically (parse hooks in `AddFiltersFromString`, query hooks around `GetSqlString`/`GetQuery`), so manual execution is rarely needed — but the plugin manager exposes it, e.g. for testing a plugin in isolation:

```go
// Execute hooks manually
err := manager.ExecuteBeforeQuery(f, ctx)
if err != nil {
    // Handle error
}

modifiedDSL, err := manager.ExecuteBeforeParse(f, dsl)
if err != nil {
    // Handle error
}

err = manager.ExecuteAfterParse(f, dsl)
if err != nil {
    // Handle error
}

err = manager.ExecuteAfterQuery(f, ctx, result)
if err != nil {
    // Handle error
}
```

## Plugin Best Practices

### 1. Error Handling
- Always return meaningful errors
- Don't panic in plugin methods
- Handle edge cases gracefully

### 2. Performance
- Keep plugin logic lightweight
- Avoid expensive operations in hooks
- Use caching for repeated operations

### 3. Thread Safety
- Plugins should be stateless or thread-safe
- Don't modify shared state without synchronization
- Use atomic operations for counters

### 4. Configuration
- Make plugins configurable
- Provide sensible defaults
- Validate configuration in Initialize()

### 5. Logging
- Use structured logging
- Include plugin name in log messages
- Log important state changes

## Common Use Cases

### 1. Field Name Mapping
```go
func (p *FieldMappingPlugin) BeforeParse(f figo.Figo, dsl string) (string, error) {
    // Map field names
    dsl = strings.ReplaceAll(dsl, "user_id", "uid")
    dsl = strings.ReplaceAll(dsl, "created_at", "created")
    return dsl, nil
}
```

### 2. Query Logging
```go
func (p *LoggingPlugin) BeforeQuery(f figo.Figo, ctx any) error {
    log.Printf("Executing query: %s", f.GetDSL())
    return nil
}

func (p *LoggingPlugin) AfterQuery(f figo.Figo, ctx any, result interface{}) error {
    log.Printf("Query completed successfully")
    return nil
}
```

### 3. Security Validation
```go
func (p *SecurityPlugin) BeforeParse(f figo.Figo, dsl string) (string, error) {
    // Check for dangerous patterns
    if strings.Contains(dsl, "DROP") || strings.Contains(dsl, "DELETE") {
        return "", fmt.Errorf("dangerous operation detected")
    }
    return dsl, nil
}
```

### 4. Query Optimization
```go
func (p *OptimizationPlugin) AfterParse(f figo.Figo, dsl string) error {
    // Analyze parsed expressions
    clauses := f.GetClauses()
    // Apply optimizations
    return nil
}
```

## Testing Plugins

### Unit Testing
```go
func TestMyPlugin(t *testing.T) {
    plugin := NewMyPlugin()
    
    // Test initialization
    f := figo.New()
    err := plugin.Initialize(f)
    assert.NoError(t, err)
    
    // Test BeforeParse
    modifiedDSL, err := plugin.BeforeParse(f, "id=1")
    assert.NoError(t, err)
    assert.Equal(t, "idd=1", modifiedDSL)
    
    // Test other hooks...
}
```

### Integration Testing
```go
func TestPluginIntegration(t *testing.T) {
    f := figo.New()
    plugin := NewMyPlugin()
    f.RegisterPlugin(plugin)
    
    // Test with real queries
    f.AddFiltersFromString("id=1")
    f.Build(adapters.RawAdapter{})
    
    // Verify plugin effects
    sql := f.GetSqlString(nil)
    assert.Contains(t, sql, "idd")
}
```

## Plugin System Architecture

```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   DSL String    │───▶│  BeforeParse    │───▶│   Parse DSL     │
└─────────────────┘    └─────────────────┘    └─────────────────┘
                                                       │
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│  Query Result   │◀───│   AfterQuery    │◀───│  Execute Query  │
└─────────────────┘    └─────────────────┘    └─────────────────┘
                                                       │
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│  Post-Process   │◀───│   AfterParse    │◀───│   Build Query   │
└─────────────────┘    └─────────────────┘    └─────────────────┘
```

## Conclusion

The Figo Plugin System provides a powerful and flexible way to extend the query builder's functionality. By implementing the Plugin interface and using the various hooks, you can:

- Transform queries before parsing
- Add security validations
- Implement custom logging
- Optimize query performance
- Add custom business logic

The system is designed to be thread-safe, performant, and easy to use, making it perfect for enterprise applications that need custom query processing capabilities.
