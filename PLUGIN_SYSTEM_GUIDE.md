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
- **Return**: Error if processing fails
- **Example**: Validate parsed expressions, add metadata

### 4. BeforeQuery Hook
- **When**: Called before query execution
- **Purpose**: Modify query context or add pre-execution logic
- **Return**: Error if pre-processing fails
- **Example**: Add authentication checks, modify query parameters

### 5. AfterQuery Hook
- **When**: Called after query execution
- **Purpose**: Post-process query results
- **Return**: Error if post-processing fails
- **Example**: Transform results, add logging, cache results

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
    "github.com/bi0dread/figo/v3"
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

## Using Plugins

### 1. Register a Plugin

```go
// Create Figo instance
f := figo.New(figo.RawAdapter{})

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
    f := figo.New(figo.RawAdapter{})
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
    f := figo.New(figo.RawAdapter{})
    plugin := NewMyPlugin()
    f.RegisterPlugin(plugin)
    
    // Test with real queries
    f.AddFiltersFromString("id=1")
    f.Build()
    
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
