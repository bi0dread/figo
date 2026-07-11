# figo — Go Dynamic Query Builder (v3)

figo turns a compact, database-agnostic filter DSL into concrete queries for **GORM**, **raw SQL**, **MongoDB**, and **Elasticsearch**. You parse one filter string (typically straight from an HTTP query parameter) into an internal expression AST, then let an adapter render it for whichever backend you target.

```
"status=active and age<bet>(18..65) sort=created_at:desc page=skip:0,take:20"
        │
        ▼   parse → AST → adapter
  GORM │ Raw SQL │ MongoDB │ Elasticsearch
```

- **Package:** `github.com/bi0dread/figo/v3`
- **Go:** 1.23+

## Installation

```bash
go get github.com/bi0dread/figo/v3
```

## Table of contents

- [Core concept: parse once, render per adapter](#core-concept-parse-once-render-per-adapter)
- [Quick start](#quick-start)
- [The DSL](#the-dsl)
  - [Comparison operators](#comparison-operators)
  - [Pattern matching (LIKE / regex)](#pattern-matching-like--regex)
  - [Set, range, and null operators](#set-range-and-null-operators)
  - [Logical operators and precedence](#logical-operators-and-precedence)
  - [Directives: sort, page, load](#directives-sort-page-load)
  - [Value typing rules](#value-typing-rules)
- [Building filters programmatically (`AddFilter`)](#building-filters-programmatically-addfilter)
- [Adapters](#adapters)
  - [GORM](#gorm-adapter)
  - [Raw SQL](#raw-sql-adapter)
  - [MongoDB](#mongodb-adapter)
  - [Elasticsearch](#elasticsearch-adapter)
- [The `Figo` API](#the-figo-api)
- [Field safety: ignore lists & whitelist](#field-safety-ignore-lists--whitelist)
- [Naming strategies](#naming-strategies)
- [Inspecting & transforming the AST](#inspecting--transforming-the-ast)
- [Caching](#caching)
- [Performance monitoring](#performance-monitoring)
- [Plugins](#plugins)
- [Validation](#validation)
- [Batch processing](#batch-processing)
- [Input validation & repair](#input-validation--repair)
- [Concurrency](#concurrency)
- [Testing](#testing)
- [Status of features](#status-of-features)
- [Contributing](#contributing)
- [License](#license)

## Core concept: parse once, render per adapter

A `Figo` instance is a mutable filter builder. The lifecycle is always:

1. **`figo.New()`** — construct an instance (no adapter argument).
2. **`AddFiltersFromString(dsl)`** — hand it a DSL string. This stores the DSL; it does **not** parse yet. Calling it again **replaces** the previous DSL.
3. **`Build(adapter)`** — parse the DSL into an AST and select the adapter. `Build` is idempotent: calling it again rebuilds cleanly (clauses, preloads, and sort are reset from the DSL each time).
4. **`GetSqlString` / `GetQuery`** (or an adapter helper like `BuildRawSelect`) — render the query.

```go
f := figo.New()
f.AddFiltersFromString(`status="active" and age>18`)
f.Build(figo.RawAdapter{})

where, args := figo.BuildRawWhere(f)
// where: "(`status` = ? AND `age` > ?)"
// args:  []any{"active", int64(18)}
```

> **Note:** `New()` takes no adapter. Supply it at `Build(adapter)` or via `SetAdapterObject(adapter)`. Calling `Build()` with no argument keeps whatever adapter was set previously.

## Quick start

### GORM

```go
package main

import (
	"fmt"

	"github.com/bi0dread/figo/v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type User struct {
	ID     uint
	Name   string
	Age    int
	Status string
}

func main() {
	db, _ := gorm.Open(sqlite.Open("test.db"), &gorm.Config{})
	_ = db.AutoMigrate(&User{})

	f := figo.New()
	f.AddFiltersFromString(`status="active" and age>=18 sort=age:desc page=skip:0,take:20`)
	f.Build(figo.GormAdapter{})

	// Apply figo's filters, sort, and pagination onto a *gorm.DB and execute.
	var users []User
	figo.ApplyGorm(f, db.Model(&User{})).Find(&users)

	fmt.Println(users)
}
```

## The DSL

A filter is a sequence of space-separated tokens: `field<operator>value` terms, the bare keywords `and` / `or` / `not`, parentheses for grouping, and the `sort=` / `page=` / `load=` directives. Whitespace (spaces, tabs, newlines) separates tokens; put a value in double quotes to keep whitespace or operator characters literal.

```
name="John Doe" and (age>18 or vip=true) and status<nin>["banned","deleted"]
```

### Comparison operators

| Op | DSL | Meaning |
|----|-----|---------|
| `=` | `id=10` | Equals |
| `!=` | `status!="deleted"` | Not equal |
| `>` | `age>18` | Greater than |
| `>=` | `score>=80` | Greater than or equal |
| `<` | `price<100` | Less than |
| `<=` | `count<=5` | Less than or equal |

### Pattern matching (LIKE / regex)

| Op | DSL | Meaning |
|----|-----|---------|
| `=^` | `name=^"%john%"` | LIKE (`%` = any run, `_` = one char) |
| `!=^` | `name!=^"%admin%"` | NOT LIKE |
| `.=^` | `name.=^"%john%"` | Case-insensitive LIKE (ILIKE) |
| `=~` | `email=~"^[a-z]+@x\.com$"` | Regex match |
| `!=~` | `phone!=~"^\+1"` | Regex not match |

Per-backend translation of `=^`:

| Backend | Output |
|---------|--------|
| Raw SQL / GORM | `col LIKE ?` (arg `%john%`) |
| MongoDB | anchored, metachar-escaped regex `^.*john.*$` |
| Elasticsearch | `wildcard` query `*john*` (literal `*`/`?` in the value are escaped) |

### Set, range, and null operators

| Op | DSL | Meaning |
|----|-----|---------|
| `<in>` | `id<in>[1,2,3]` | Value in list |
| `<nin>` | `status<nin>["a","b"]` | Value not in list |
| `<bet>` | `price<bet>(10..100)` | Inclusive range |
| `<null>` | `deleted_at<null>` | IS NULL |
| `<notnull>` | `updated_at<notnull>` | IS NOT NULL |

`x=null` and `x!=null` are shorthand for `<null>` / `<notnull>` — an unquoted `null` value becomes IS NULL / IS NOT NULL rather than a comparison against a literal.

An **empty** `<in>[]` list is safe on every adapter: it renders a match-nothing predicate (SQL `1=0`, ES `match_none`, Mongo `$in: []`) instead of dropping the condition. Empty `<nin>[]` matches everything.

### Logical operators and precedence

`and`, `or`, `not` combine terms. Precedence, highest to lowest:

1. `not`
2. `and`
3. `or`

```go
f.AddFiltersFromString(`a=1 and b=2 or c=3`)
// (a=1 AND b=2) OR c=3

f.AddFiltersFromString(`not deleted=true and active=true`)
// (NOT deleted=true) AND active=true

f.AddFiltersFromString(`(a=1 or a=2) and (b=3 or b=4)`)
// parentheses override precedence
```

**`not` semantics are uniform across adapters:** `not` over multiple operands means *none of them match* — `NOT (a OR b)` in SQL, `$nor` in MongoDB, `must_not` over all operands in Elasticsearch, `clause.Not` in GORM. The DSL only ever produces single-operand `not`; multi-operand forms come from building the AST directly with `AddFilter`.

A leading `not` (`not deleted=true`) is valid and preserved across all entry paths, including the repair path.

### Directives: sort, page, load

| Directive | DSL | Effect |
|-----------|-----|--------|
| `sort=` | `sort=name:asc,created_at:desc` | Ordering (multiple columns, comma-separated) |
| `page=` | `page=skip:10,take:5` | Pagination (skip/offset + take/limit) |
| `load=` | `load=[Orders:total>100 \| Profile:bio=^"%dev%"]` | Preloads / joins with their own filters |

`load=` segments are separated by `|`; each is `Relation:filter`, where `filter` is itself a DSL expression. `take:0` and `skip:0` mean "no limit"/"no offset" consistently across adapters (GORM will **not** emit `LIMIT 0`).

Field names that merely *start* with a directive keyword (`sortOrder`, `pageCount`, `loadedAt`) are treated as ordinary fields — the `=` after the keyword is required for it to be a directive.

### Value typing rules

figo types each literal exactly once, and **quoting is how you keep a value a string**:

| DSL | Parsed value | Go type |
|-----|--------------|---------|
| `id=123` | `123` | `int64` |
| `price=9.99` | `9.99` | `float64` |
| `active=true` | `true` | `bool` |
| `x=null` | IS NULL predicate | — |
| `created=2023-01-02` | `2023-01-02` parsed | `time.Time` |
| `code="0123"` | `"0123"` | `string` (quoting preserves it) |
| `flag="true"` | `"true"` | `string` |
| `status=active` | `"active"` | `string` (unquoted, non-numeric) |
| `id<in>[1,"2",3]` | `[1, "2", 3]` | `[]any` (per-element typing) |

Consequences worth knowing:

- **Quoted numeric-looking values stay strings** — `code="0123"` is `"0123"`, not `123`. Use this for zip codes, phone numbers, and IDs with leading zeros.
- **Unquoted dates** in common formats (`2006-01-02`, RFC3339, `01/02/2006`, …) parse to `time.Time`. Date format detection tries US (`MM/DD/YYYY`) before EU (`DD/MM/YYYY`) for ambiguous slash dates.
- **Integers larger than int64** are kept as strings rather than silently degrading to a lossy `float64`.

`ParseFieldsValue(str)` exposes this same single-value typing if you need it outside the DSL (e.g. to coerce one incoming parameter the way figo would):

```go
f.ParseFieldsValue("123")       // int64(123)
f.ParseFieldsValue(`"123"`)     // "123" (quoted -> string)
f.ParseFieldsValue("true")      // true
f.ParseFieldsValue("2023-01-02") // time.Time
```

## Building filters programmatically (`AddFilter`)

Sometimes you don't want to build a DSL string — you already have typed values (from a struct, a form, another query layer) and want to add conditions directly. `AddFilter(exp Expr)` appends a node to the AST, bypassing the parser. You can use it on its own or mix it with a DSL.

```go
f := figo.New()
f.AddFilter(figo.EqExpr{Field: "status", Value: "active"})
f.AddFilter(figo.BetweenExpr{Field: "age", Low: int64(18), High: int64(65)})
f.Build(figo.RawAdapter{})

where, args := figo.BuildRawWhere(f)
// where: "`status` = ? AND `age` BETWEEN ? AND ?"
// args:  []any{"active", int64(18), int64(65)}
```

Multiple `AddFilter` clauses are combined with **AND** at the top level.

### Expression types

Every node implements the `figo.Expr` interface. Values are used as-is (no re-parsing), so pass real Go types.

| Constructor | Fields | Renders as |
|-------------|--------|------------|
| `EqExpr` | `Field, Value any` | `field = ?` |
| `NeqExpr` | `Field, Value any` | `field != ?` |
| `GtExpr` / `GteExpr` | `Field, Value any` | `field > ?` / `>=` |
| `LtExpr` / `LteExpr` | `Field, Value any` | `field < ?` / `<=` |
| `LikeExpr` | `Field, Value any` | `field LIKE ?` |
| `ILikeExpr` | `Field, Value any` | case-insensitive LIKE |
| `RegexExpr` | `Field, Value any` | regex match |
| `InExpr` | `Field string, Values []any` | `field IN (…)` |
| `NotInExpr` | `Field string, Values []any` | `field NOT IN (…)` |
| `BetweenExpr` | `Field string, Low, High any` | `field BETWEEN ? AND ?` |
| `IsNullExpr` | `Field string` | `field IS NULL` |
| `NotNullExpr` | `Field string` | `field IS NOT NULL` |
| `AndExpr` | `Operands []Expr` | `(a AND b AND …)` |
| `OrExpr` | `Operands []Expr` | `(a OR b OR …)` |
| `NotExpr` | `Operands []Expr` | `NOT (a OR b …)` — none of the operands match |

Nest the logical types to express any structure:

```go
// (role = "admin" OR role = "mod") AND NOT (banned = true)
f.AddFilter(figo.AndExpr{Operands: []figo.Expr{
	figo.OrExpr{Operands: []figo.Expr{
		figo.EqExpr{Field: "role", Value: "admin"},
		figo.EqExpr{Field: "role", Value: "mod"},
	}},
	figo.NotExpr{Operands: []figo.Expr{
		figo.EqExpr{Field: "banned", Value: true},
	}},
}})
```

### Ordering matters: call `AddFilter` *after* `Build()`

`Build()` recompiles the AST from the DSL and, when a DSL is present, **resets the clause list** first. So an `AddFilter` call made *before* `Build()` is discarded if there's also a DSL. Two safe patterns:

```go
// A) No DSL — AddFilter only. Build() with an empty DSL keeps your clauses.
f := figo.New()
f.AddFilter(figo.EqExpr{Field: "status", Value: "active"})
f.Build(figo.RawAdapter{})            // clauses preserved

// B) DSL + programmatic — add AFTER Build so it isn't wiped.
f := figo.New()
f.AddFiltersFromString(`name="x"`)
f.Build(figo.RawAdapter{})
f.AddFilter(figo.InExpr{Field: "role", Values: []any{"admin", "mod"}})
where, _ := figo.BuildRawWhere(f)     // "`name` = ? AND `role` IN (?,?)"
```

`AddFilter` clauses are still subject to the [ignore list and whitelist](#field-safety-ignore-lists--whitelist) — a disallowed or ignored field is pruned just as it would be from DSL input.

## Adapters

All adapters consume the same AST. Pass one to `Build()` (or `SetAdapterObject`), then use `GetSqlString` / `GetQuery` or the adapter's package-level helpers.

`GetQuery(ctx)` returns a backend-specific value (all implement the `figo.Query` interface); type-assert to the concrete type:

| Adapter | `ctx` argument | `GetQuery` returns |
|---------|----------------|--------------------|
| `RawAdapter{}` | table name `string` or `RawContext{Table}` | `SQLQuery{SQL, Args}` |
| `GormAdapter{}` | `*gorm.DB` | `SQLQuery{SQL, Args}` |
| `MongoAdapter{}` | `nil`, or `"AGG"` + joins for aggregation | `MongoFindQuery{Filter, Options}` / `MongoAggregateQuery{Pipeline, Options}` |
| `ElasticsearchAdapter{}` | `nil` | `ElasticsearchQueryWrapper{Query}` |

### GORM adapter

```go
f := figo.New()
f.AddFiltersFromString(`status="active" and age>=18 sort=age:desc page=skip:0,take:20`)
f.Build(figo.GormAdapter{})

// Option A: apply onto a *gorm.DB and execute yourself
var users []User
figo.ApplyGorm(f, db.Model(&User{})).Find(&users)

// Option B: render the SQL string (DryRun) for logging/inspection
sql := f.GetSqlString(db.Model(&User{}))           // full SELECT
where := f.GetSqlString(db.Model(&User{}), "WHERE") // just the WHERE segment

// Option C: get placeholder SQL + args
q := f.GetQuery(db.Model(&User{})).(figo.SQLQuery)
// q.SQL, q.Args
```

`ApplyGorm` sets limit/offset, select fields, preloads, where clauses, and sort. A `*gorm.DB` that already carries a caller scope (e.g. a tenant filter `db.Where("org_id = ?", id)`) keeps that scope — figo's filters are applied **on top of** it, not instead of it.

### Raw SQL adapter

Targets MySQL/SQLite dialect (backtick identifiers, `?` placeholders).

```go
f := figo.New()
f.AddFiltersFromString(`id=1 and name="test" sort=id:desc page=skip:0,take:20`)
f.Build(figo.RawAdapter{})

// Full SELECT
sql, args := figo.BuildRawSelect(f, "users")
// sql:  "SELECT * FROM `users` WHERE (`id` = ? AND `name` = ?) ORDER BY `id` DESC LIMIT 20"
// args: []any{int64(1), "test"}

// Just the WHERE fragment
where, whereArgs := figo.BuildRawWhere(f)

// Or via the generic API with a table name / RawContext
sql = f.GetSqlString("users")
sql = f.GetSqlString(figo.RawContext{Table: "users"}, "SELECT", "FROM", "WHERE", "SORT")
q := f.GetQuery(figo.RawContext{Table: "users"}).(figo.SQLQuery) // q.SQL + q.Args
```

Identifiers are backtick-escaped (values are always parameterized), so field/table names can't break out of quoting.

`load=` preloads are exposed for the raw adapter as rendered `WHERE` fragments you can apply to your own join/second-query logic:

```go
f.AddFiltersFromString(`id>0 load=[Orders:total>100]`)
f.Build(figo.RawAdapter{})
preloads := figo.BuildRawPreloads(f)          // map[string]RawPreload
// preloads["Orders"] == RawPreload{Where: "`total` > ?", Args: []any{int64(100)}}
```

### MongoDB adapter

```go
f := figo.New()
f.AddFiltersFromString(`status="active" and age>=18 sort=age:desc page=skip:0,take:20`)
f.Build(figo.MongoAdapter{})

// Filter + find options directly
filter, err := figo.BuildMongoFilter(f)   // bson.M
opts := figo.BuildMongoFindOptions(f)      // *options.FindOptions (sort/limit/skip)

// Or via the generic API — returns MongoFindQuery
q := f.GetQuery(nil).(figo.MongoFindQuery) // q.Filter, q.Options

// Aggregation pipeline with joins ($lookup) — pass "AGG" and a joins map
joins := map[string]figo.MongoJoin{
	"orders": {From: "orders", LocalField: "_id", ForeignField: "user_id", As: "orders"},
}
pipeline, err := figo.BuildMongoAggregatePipeline(f, joins)
```

`$in`/`$nin` always receive a real array (never `null`), so empty-list filters don't error at the server.

### Elasticsearch adapter

```go
f := figo.New()
f.AddFiltersFromString(`name=^"%john%" and age>=18 sort=age:desc page=skip:0,take:20`)
f.AddSelectFields("id", "name", "age")
f.Build(figo.ElasticsearchAdapter{})

query, err := figo.BuildElasticsearchQuery(f) // figo.ElasticsearchQuery (Query/Sort/From/Size/Source)

// Or via the generic API — returns ElasticsearchQueryWrapper
q := f.GetQuery(nil).(figo.ElasticsearchQueryWrapper) // q.Query is the ElasticsearchQuery

// JSON string forms
jsonStr, err := figo.GetElasticsearchQueryString(f)         // pretty
compact, err := figo.GetElasticsearchQueryStringCompact(f)  // compact

// Fluent builder
q := figo.NewElasticsearchQueryBuilder().
	FromFigo(f).
	AddSort("name", true).
	SetPagination(0, 10).
	SetSource("id", "name").
	Build()
```

`AddSelectFields` maps to `_source`, `page=` maps to `from`/`size`, `sort=` maps to the ES sort array.

## The `Figo` API

`figo.New()` returns the `Figo` interface. The most commonly used methods:

**Filters & building**

```go
AddFiltersFromString(dsl string) error
AddFiltersFromStringWithRepair(dsl string, useRepair bool) error
AddFilter(exp Expr)                 // add a programmatic AST node
Build(adapter ...Adapter)
GetClauses() []Expr
GetPreloads() map[string][]Expr
GetDSL() string
```

**Rendering**

```go
GetSqlString(ctx any, conditionType ...string) string
GetQuery(ctx any, conditionType ...string) Query
GetExplainedSqlString(ctx any, conditionType ...string) string
GetCachedSqlString(ctx any, conditionType ...string) string
GetCachedQuery(ctx any, conditionType ...string) Query
```

**Pagination & sorting**

```go
SetPage(skip, take int)
SetPageString(v string)             // "skip:10,take:5"
GetPage() Page                      // returns a copy — use SetPage to change it
GetSort() *OrderBy
```

**Adapter & inspection**

```go
SetAdapterObject(adapter Adapter)
GetAdapterObject() Adapter
Explain() string                    // human-readable AST tree
Clone() Figo                        // deep copy
Walk(visit func(Expr))              // traverse/mutate the AST
```

> `GetPage()` returns a **copy** of the page. Mutating it has no effect — call `SetPage(skip, take)` to change pagination.

## Field safety: ignore lists & whitelist

Because DSL usually comes from untrusted input, figo gives you two ways to constrain which fields a caller may filter on. Configure them **before** adding filters. Both prune the built AST — dropping a condition never leaves a dangling `and`/`or`/`not` behind, and both apply to DSL filters and to programmatic `AddFilter` clauses alike.

**Ignore list** — silently drop specific fields:

```go
f := figo.New()
f.AddIgnoreFields("password", "internal_notes")
f.AddFiltersFromString(`name="x" and password="y"`)
f.Build(figo.RawAdapter{})
// only name survives
```

Ignore names match both the raw and naming-converted spelling, so `AddIgnoreFields("user_name")` also blocks `userName` under the snake_case strategy.

**Whitelist** — allow *only* listed fields:

```go
f := figo.New()
f.SetAllowedFields("name", "age", "status")
f.EnableFieldWhitelist()
f.AddFiltersFromString(`name="x" and secret="y"`) // secret is dropped
f.Build(figo.RawAdapter{})

// Also: DisableFieldWhitelist(), IsFieldAllowed(field), IsFieldWhitelistEnabled(), GetAllowedFields()
```

**Select fields** — restrict returned columns (SQL `SELECT`, ES `_source`):

```go
f.AddSelectFields("id", "name", "email")
```

## Naming strategies

By default figo converts DSL field names to `snake_case` (so `userName` → `user_name`). You can change the strategy or supply an arbitrary function.

```go
f.SetNamingStrategy(figo.NAMING_STRATEGY_NO_CHANGE) // keep names verbatim
f.SetNamingStrategy(figo.NAMING_STRATEGY_SNAKE_CASE) // default

// Custom function overrides the strategy entirely, for the DSL and every adapter's
// column normalization (they no longer disagree):
f.SetNamingFunc(func(field string) string {
	return "t_" + field
})
```

The regex SQL operator for `=~`/`!=~` is a **package-level** setting (it affects the Raw and GORM adapters process-wide) and is safe to change concurrently:

```go
figo.SetRegexSQLOperator("REGEXP") // MySQL / SQLite (default)
figo.SetRegexSQLOperator("~")      // PostgreSQL, case-sensitive
figo.SetRegexSQLOperator("~*")     // PostgreSQL, case-insensitive
op := figo.GetRegexSQLOperator()
```

## Inspecting & transforming the AST

**`Explain()`** renders the parsed AST as an indented tree — handy for debugging precedence/grouping without a database:

```go
f.AddFiltersFromString(`a=1 and (b=2 or c=3)`)
f.Build(figo.RawAdapter{})
fmt.Println(f.Explain())
```

**`Clone()`** deep-copies an instance (clauses, preloads, and nested operand slices are independent — mutating the clone never affects the original).

**`Walk(visit)`** traverses every clause and preload, letting you rewrite nodes in place. The visitor receives a pointer to each node; use `NodeField` / `SetNodeField` to read and rewrite field names generically. `Walk` rebuilds operand slices rather than mutating shared ones, and runs your visitor outside its internal lock (so a visitor may call other `Figo` methods safely).

```go
// Qualify every field with a table prefix
f.Walk(func(n figo.Expr) {
	if field, ok := figo.NodeField(n); ok {
		figo.SetNodeField(n, "users."+field)
	}
})
```

## Caching

figo can cache rendered SQL/query results keyed by the full instance state (DSL, clauses, page, sort, field sets, naming, adapter type, regex operator, context).

```go
f.SetCacheConfig(figo.CacheConfig{
	Enabled:         true,
	TTL:             5 * time.Minute,
	MaxSize:         1000,          // 0 = unlimited; LRU eviction at capacity
	CleanupInterval: time.Minute,   // background expiry sweep
})

sql := f.GetCachedSqlString(figo.RawContext{Table: "users"})
q := f.GetCachedQuery(figo.RawContext{Table: "users"})

stats := f.GetCacheStats() // hits, misses, size, hit rate
f.ClearCache()

// Or inject your own implementation of the QueryCache interface:
f.SetCache(myCache)
```

Cache keys are type-aware — `a = int64(1)` and `a = "1"` never collide even when instances share a cache. A cache created via `SetCacheConfig` stops its background goroutine when it's replaced or when the instance is garbage-collected. `NewInMemoryCache(config)` is available if you want to manage one directly (call `Stop()` when done).

## Performance monitoring

```go
mon := figo.NewPerformanceMonitor(true)
f.SetPerformanceMonitor(mon)

// ... run cached queries ...

m := f.GetMetrics()
// m.QueryCount, m.CacheHits, m.CacheMisses, m.AverageLatency, m.ErrorCount, ...
f.ResetMetrics()
```

Metrics are recorded on the `GetCachedSqlString` / `GetCachedQuery` paths.

## Plugins

Register plugins to hook into the parse pipeline. Each plugin implements `Name`, `Version`, `Initialize`, `BeforeParse`, `AfterParse`, `BeforeQuery`, `AfterQuery`.

```go
f.RegisterPlugin(myPlugin)   // Initialize is called; rolled back if it errors
f.UnregisterPlugin("my-plugin")
```

The `BeforeParse` / `AfterParse` hooks fire automatically inside `AddFiltersFromString` (`BeforeParse` can rewrite the DSL). Hooks run on a snapshot outside the manager's lock, so a hook may call back into the manager without deadlocking.

> **Wiring status:** only the parse hooks (`BeforeParse` / `AfterParse`) are invoked automatically today. `BeforeQuery` / `AfterQuery` exist on the interface and can be driven manually via the plugin manager, but are not yet auto-invoked by the query path.

## Validation

A validation manager lets you attach rules to fields. Built-in validators include `required`, `min_length`, and `email`.

```go
vm := figo.NewValidationManager()
vm.RegisterValidator(figo.RequiredValidator{})
f.SetValidationManager(vm)

f.AddValidationRule(figo.ValidationRule{Field: "email", Rule: "email", Message: "invalid email"})
if err := f.ValidateField("email", "not-an-email"); err != nil {
	// handle
}
```

Rule handlers run on a snapshot (a handler may safely call back into the manager). Validation is invoked explicitly via `ValidateField` — it is not run automatically during parse/build.

## Batch processing

Run many independent figo queries with bounded concurrency and an optional per-operation timeout.

```go
bp := figo.NewInMemoryBatchProcessor(8, 2*time.Second) // max 8 concurrent, 2s timeout

ops := []figo.BatchOperation{
	{ID: "a", Type: "sql", Query: f1, Context: figo.RawContext{Table: "users"}},
	{ID: "b", Type: "query", Query: f2, Context: nil},
}
results := bp.Process(ops)          // []BatchResult (blocking)
ch := bp.ProcessAsync(ops)          // <-chan BatchResult (streaming)
```

`Type` is one of `"sql"`, `"query"`, `"cached_sql"`, `"cached_query"`. The concurrency cap is honored even when operations time out.

## Input validation & repair

`AddFiltersFromString` stores input as-is. `AddFiltersFromStringWithRepair` gives you validation and optional auto-repair of common malformation:

```go
// Validate and attempt repair
err := f.AddFiltersFromStringWithRepair(`(name="john" and age>25`, true)  // adds missing ')'

// Validate only, reject on malformation
err = f.AddFiltersFromStringWithRepair(`name = = 5`, false)

// Structured errors
if perr, ok := err.(*figo.ParseError); ok {
	fmt.Printf("%s at line %d col %d\n", perr.Message, perr.Line, perr.Column)
}
```

Repairs cover unmatched parentheses/quotes/brackets and dangling trailing/leading `and`/`or`. A leading `not` is **not** treated as malformed and is never stripped.

## Concurrency

A `Figo` instance is guarded by an internal `sync.RWMutex`, and the ancillary managers (cache, plugins, validation, performance monitor) each carry their own lock. Read-render methods (`GetSqlString`, `GetQuery`, `GetCached*`) are safe to call concurrently after `Build`, and the package is race-clean under `go test -race`.

For concurrent **writers** — multiple goroutines calling `AddFiltersFromString`/`Build` on the *same* instance — prefer giving each goroutine its own instance (or a `Clone()`), since those mutate shared builder state. The safe, common pattern:

```go
base := figo.New()
base.AddFiltersFromString(`status="active"`)
base.Build(figo.RawAdapter{})

var wg sync.WaitGroup
for i := 0; i < 10; i++ {
	wg.Add(1)
	go func() {
		defer wg.Done()
		// concurrent reads on the same built instance are safe
		_ = base.GetSqlString(figo.RawContext{Table: "users"})
	}()
}
wg.Wait()
```

## Testing

```bash
go test ./...            # unit + adapter tests
go test -race ./...      # race detector
```

The MongoDB adapter tests use the BSON encoder directly (no server needed). Elasticsearch **integration** tests require a running cluster on `localhost:9200` and skip automatically when it's absent — see [ELASTICSEARCH_TESTING.md](ELASTICSEARCH_TESTING.md) for the Docker Compose setup.

## Status of features

Fully wired end-to-end: the DSL and all operators above, the four adapters, ignore/whitelist/select field controls, naming strategies, pagination/sort/preloads, caching, performance monitoring, batch processing, validation (manual), input repair, and the `Explain`/`Clone`/`Walk` AST tools.

Partial / not yet wired (defined in the API but not auto-invoked or without adapter support):

- Plugin `BeforeQuery` / `AfterQuery` hooks — present but not auto-called by the query path (parse hooks are).
- `QueryLimits` (`SetQueryLimits`) — configurable but not currently enforced during parse/build.
- Advanced expression types (`JsonPathExpr`, `ArrayContainsExpr`, `ArrayOverlapsExpr`, `FullTextSearchExpr`, `GeoDistanceExpr`, `CustomExpr`) — defined for programmatic `AddFilter`, but adapters return an "unsupported expression" error for them rather than rendering.

## Contributing

Pull requests welcome:

1. Branch off `main` (don't PR against `main` directly from `main`).
2. Include tests covering your change; keep `go test -race ./...` green.
3. Update this README when you change behavior or the API.

## License

BSD 2-Clause. See [LICENSE](LICENSE).
