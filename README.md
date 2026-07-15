# figo — Go Dynamic Query Builder (v4)

figo turns a compact, database-agnostic filter DSL into concrete queries for **GORM**, **raw SQL** (MySQL / PostgreSQL / SQLite dialects), **MongoDB**, and **Elasticsearch**. You parse one filter string (typically straight from an HTTP query parameter) into an internal expression AST, then let an adapter render it for whichever backend you target.

The core stays deliberately small — parse, build, render. Everything policy-shaped is an opt-in **plugin**: syntax validation & repair, field whitelisting, complexity limits, value validation, mandatory scopes (multi-tenant), caching, metrics, and audit logging.

```
"status=active and age<bet>(18..65) sort=created_at:desc page=skip:0,take:20"
        │
        ▼   parse → AST → adapter
  GORM │ Raw SQL │ MongoDB │ Elasticsearch
```

- **Package:** `github.com/bi0dread/figo/v4`
- **Go:** 1.23+

## Installation

```bash
go get github.com/bi0dread/figo/v4
```

The adapters and the built-in plugins live in subpackages:

```go
import (
	figo "github.com/bi0dread/figo/v4"          // core: parse, build, render, plugin SPI
	"github.com/bi0dread/figo/v4/adapters"      // RawAdapter, GormAdapter, MongoAdapter, ElasticsearchAdapter
	"github.com/bi0dread/figo/v4/plugins"       // the eight built-in plugins
)
```

## What's new in v4 (migrating from v3)

v4 is a re-architecture around a small core and an explicit plugin system. The parse → build → render flow is unchanged, but construction, rendering entry points, and every policy feature moved. Skim the bullets for the APIs you use.

### Breaking changes

- **Breaking: `New()` no longer takes an adapter.** The adapter now belongs to the render step, not construction — pass it to `Build(adapter)` (or set it with `SetAdapterObject`). This lets one parsed instance be rebuilt against different backends.

  ```go
  // v3
  f := figo.New(figo.GormAdapter{})
  f.AddFiltersFromString(`status="active"`)
  f.Build()

  // v4
  f := figo.New()
  f.AddFiltersFromString(`status="active"`)
  f.Build(adapters.GormAdapter{})
  ```

- **Breaking: `Build` takes exactly one adapter, not a variadic.** The signature is now `Build(adapter Adapter)`. Passing a non-nil adapter selects it; pass `Build(nil)` to rebuild while keeping the adapter set by an earlier `Build`/`SetAdapterObject` (this replaces the old no-argument `Build()`).

  ```go
  // v3 / early v4: variadic, no-arg rebuild kept the previous adapter
  f.Build()

  // v4: single adapter; use nil to rebuild in place
  f.Build(nil)
  ```

- **Breaking: the core validation manager is now a plugin.** `NewValidationManager`, `SetValidationManager`, `GetValidationManager`, `AddValidationRule`, `RegisterValidator` and `ValidateField` are removed from `Figo`. Use `NewValidationPlugin()` + `f.RegisterPlugin(vp)` instead — see [Validation](#validation). Rules, validators, and the built-ins (`required`, `min_length`, `email`) are unchanged.

- **Breaking: query caching is now a plugin.** `SetCache`, `GetCache`, `SetCacheConfig`, `GetCacheConfig`, `GetCacheStats`, `ClearCache`, `GetCachedSqlString` and `GetCachedQuery` are removed from `Figo`. Use `NewCachePlugin(config)` and render through it: `cp.GetCachedSqlString(f, ctx)` — see [Caching](#caching). `QueryCache`, `CacheConfig`, `CacheStats` and `NewInMemoryCache` are unchanged, and one plugin can now serve many instances.

- **Breaking: performance monitoring is now a plugin.** `SetPerformanceMonitor`, `GetPerformanceMonitor`, `GetMetrics` and `ResetMetrics` are removed from `Figo`. Use `NewMetricsPlugin(true)` (or a bare `NewPerformanceMonitor`) and attach it to the cache plugin: `cp.SetPerformanceMonitor(mp.PerformanceMonitor)` — see [Performance monitoring](#performance-monitoring). `PerformanceMonitor` and `Metrics` themselves are unchanged.

- **Breaking: field policy (ignore list & whitelist) is now a plugin.** `AddIgnoreFields`, `SetAllowedFields`, `EnableFieldWhitelist`, `DisableFieldWhitelist`, `IsFieldAllowed`, `GetIgnoreFields`, `GetAllowedFields` and `IsFieldWhitelistEnabled` are removed from `Figo`. Use `NewFieldsPlugin()` + `f.RegisterPlugin(fp)` — see [Field safety](#field-safety-ignore-lists--whitelist). Enforcement is unchanged (Build and AddFilter, DSL and programmatic filters alike) via the new `ExprFilter` plugin hook, which custom plugins can implement too. `AddSelectFields` / `GetSelectFields` stay on the instance. **Note:** without a registered `FieldsPlugin`, no ignore/whitelist pruning happens.

- **Breaking: query limits are now a plugin — and actually enforced.** `SetQueryLimits` / `GetQueryLimits` are removed from `Figo` (they stored limits that were never checked). `NewLimitsPlugin(plugins.DefaultQueryLimits())` + `f.RegisterPlugin(lp)` now really enforces nesting/field/parameter/expression limits on every parsed DSL — see [Query complexity limits](#query-complexity-limits).

- **Breaking: naming is a single `NamingFunc` — the strategy enum is gone.** `NamingStrategy`, `NAMING_STRATEGY_*`, `SetNamingStrategy` and `GetNamingStrategy` are removed. The built-ins are now `NamingFunc` values: `figo.SnakeCaseNaming` (still the default) and `figo.NoChangeNaming` — set them with the existing `SetNamingFunc`. `SetNamingFunc(nil)` resets to the default; `GetNamingFunc()` never returns nil.

- **Breaking: `ParseFieldsValue` is now the package-level `figo.ParseValue`.** The method never used any instance state — it's the DSL's literal typer. Replace `f.ParseFieldsValue(s)` with `figo.ParseValue(s)`; the typing rules are identical.

- **Breaking: `AddFiltersFromStringWithRepair` is now the `SyntaxPlugin`.** The method is removed from `Figo`; register `plugins.NewSyntaxPlugin(false)` (validate strictly) or `plugins.NewSyntaxPlugin(true)` (attempt repair first) and use plain `AddFiltersFromString` — see [Input validation & repair](#input-validation--repair). Same checks, same `*ParseError` (now wrapped, so match with `errors.As`).

- **Breaking: `GetExplainedSqlString` is removed.** It was a byte-for-byte duplicate of `GetSqlString` — call that instead (identical output). For a human-readable view of the parsed AST, use `Explain()`.

- **Breaking: batch processing is removed.** `BatchOperation`, `BatchResult`, `BatchProcessor` and `NewInMemoryBatchProcessor` are gone. Rendering a query string is fast, synchronous CPU work — if you need to render many queries concurrently, a few lines of `errgroup`/goroutines over `f.GetSqlString(ctx)` replace the whole feature with better types and real context cancellation.

- **Breaking: the adapters live in the `adapters` subpackage.** `RawAdapter`, `GormAdapter`, `MongoAdapter`, `ElasticsearchAdapter` — plus their helpers and types (`BuildRawWhere`, `BuildRawSelect`, `RawContext`, `BuildMongoFilter`, `MongoJoin`, `BuildElasticsearchQuery`, the `SQLDialect`s, …) — moved from the root package to `github.com/bi0dread/figo/v4/adapters`. The `Adapter` interface, `Query`, and `SQLQuery` stay in the core. As part of this, the `Query` marker method is now exported (`IsQuery()`), which also makes custom third-party `Query` result types possible for the first time. Migration is an import + prefix change: `f.Build(adapters.GormAdapter{})`.

- **Breaking: the built-in plugins live in the `plugins` subpackage.** `ValidationPlugin`, `CachePlugin`, `MetricsPlugin`, `FieldsPlugin`, `LimitsPlugin`, `SyntaxPlugin`, `ScopePlugin`, `AuditPlugin` — plus their types (`ValidationRule`, `CacheConfig`, `QueryLimits`, `PerformanceMonitor`, `InMemoryCache`, …) — moved from the root package to `github.com/bi0dread/figo/v4/plugins`. The plugin *SPI* (`Plugin`, `PluginManager`, `ExprFilter`, `ClauseFinalizer`, `RegisterPlugin`) stays in the core, so custom plugins are unaffected. Migration is an import + prefix change:

  ```go
  import "github.com/bi0dread/figo/v4/plugins"

  fp := plugins.NewFieldsPlugin() // was: figo.NewFieldsPlugin()
  f.RegisterPlugin(fp)
  ```

### New in v4

- **New: `ScopePlugin` and `AuditPlugin`.** Mandatory query scoping (multi-tenant row security, via the new `FinalizeClauses` plugin hook that runs on every Build) and parse/render audit logging — see [Mandatory scopes](#mandatory-scopes-multi-tenant) and [Auditing](#auditing).

- **New: `RawAdapter` is dialect-aware.** `RawAdapter{Dialect: adapters.PostgresDialect}` renders `"col"` identifiers, `$1..$N` placeholders, and the `~` regex operator; `adapters.SQLiteDialect` renders `"col"` with `?`; the zero value keeps the MySQL rendering (backticks, `?`, `REGEXP`) unchanged. String-literal escaping is per-dialect too (backslash doubling only on MySQL). **Behavior note:** the raw adapter's regex operator now comes from the dialect — `SetRegexSQLOperator` only affects the GORM adapter.

- **New: `BeforeQuery` / `AfterQuery` plugin hooks are now auto-invoked** around every `GetSqlString` / `GetQuery` render (previously they existed on the interface but never fired). A hook error vetoes the render — if you have a plugin whose `BeforeQuery` returns errors, it now actually blocks queries.

- **New: `Walk`** — traverse and rewrite the built AST (see [Inspecting & transforming the AST](#inspecting--transforming-the-ast)).
- **40+ bug fixes** from two deep audits: quote-aware parser tokenizing, exact keyword matching, LIKE/regex translation on Mongo/ES, empty-`<in>` filter-bypass, SQL-injection hardening on identifiers, cache races/LRU/expiry, deterministic SQL output, and Mongo/ES now **error** on unsupported expressions instead of silently dropping them.

Import path changes to `github.com/bi0dread/figo/v4`. The parse/build/render core (`AddFiltersFromString`, `AddFilter`, `Build`, `GetSqlString`, `GetQuery`, paging/sort/selects, `Explain`/`Clone`/`Walk`) is source-compatible with late v3 apart from the bullets above; everything that moved to a plugin needs the one-line registration shown in its section.

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
  - [Writing your own adapter](#writing-your-own-adapter)
- [The `Figo` API](#the-figo-api)
- [Field safety: ignore lists & whitelist](#field-safety-ignore-lists--whitelist)
- [Query complexity limits](#query-complexity-limits)
- [Mandatory scopes (multi-tenant)](#mandatory-scopes-multi-tenant)
- [Auditing](#auditing)
- [Naming](#naming)
- [Inspecting & transforming the AST](#inspecting--transforming-the-ast)
- [Caching](#caching)
- [Performance monitoring](#performance-monitoring)
- [Plugins](#plugins)
- [Validation](#validation)
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
f.Build(adapters.RawAdapter{})

where, args := adapters.BuildRawWhere(f)
// where: "(`status` = ? AND `age` > ?)"
// args:  []any{"active", int64(18)}
```

> **Note:** `New()` takes no adapter. Supply it at `Build(adapter)` or via `SetAdapterObject(adapter)`. `Build` takes exactly one adapter; pass `Build(nil)` to rebuild against whatever adapter was set previously (by an earlier `Build` or `SetAdapterObject`).

**Defaults set by `New()`:** pagination starts at `skip:0, take:20` — a query with no `page=` directive is limited to 20 rows. Use `page=` in the DSL or `SetPage(skip, take)` to change it (`take:0` = no limit). Naming defaults to snake_case (`figo.SnakeCaseNaming`).

## Quick start

### GORM

```go
package main

import (
	"fmt"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
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
	f.Build(adapters.GormAdapter{})

	// Apply figo's filters, sort, and pagination onto a *gorm.DB and execute.
	var users []User
	adapters.ApplyGorm(f, db.Model(&User{})).Find(&users)

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

The package-level `figo.ParseValue(str)` exposes this same single-value typing if you need it outside the DSL (e.g. to coerce one incoming parameter the way figo would):

```go
figo.ParseValue("123")        // int64(123)
figo.ParseValue(`"123"`)      // "123" (quoted -> string)
figo.ParseValue("true")       // true
figo.ParseValue("2023-01-02") // time.Time
```

## Building filters programmatically (`AddFilter`)

Sometimes you don't want to build a DSL string — you already have typed values (from a struct, a form, another query layer) and want to add conditions directly. `AddFilter(exp Expr)` appends a node to the AST, bypassing the parser. You can use it on its own or mix it with a DSL.

```go
f := figo.New()
f.AddFilter(figo.EqExpr{Field: "status", Value: "active"})
f.AddFilter(figo.BetweenExpr{Field: "age", Low: int64(18), High: int64(65)})
f.Build(adapters.RawAdapter{})

where, args := adapters.BuildRawWhere(f)
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
f.Build(adapters.RawAdapter{})            // clauses preserved

// B) DSL + programmatic — add AFTER Build so it isn't wiped.
f := figo.New()
f.AddFiltersFromString(`name="x"`)
f.Build(adapters.RawAdapter{})
f.AddFilter(figo.InExpr{Field: "role", Values: []any{"admin", "mod"}})
where, _ := adapters.BuildRawWhere(f)     // "`name` = ? AND `role` IN (?,?)"
```

`AddFilter` clauses are still subject to a registered `FieldsPlugin`'s [ignore list and whitelist](#field-safety-ignore-lists--whitelist) — a disallowed or ignored field is pruned just as it would be from DSL input.

## Adapters

The four adapters live in the `adapters` subpackage (`import "github.com/bi0dread/figo/v4/adapters"`). All consume the same AST. Pass one to `Build()` (or `SetAdapterObject`), then use `GetSqlString` / `GetQuery` or the adapter's package-level helpers.

`GetQuery(ctx)` returns a backend-specific value (all implement the `figo.Query` interface); type-assert to the concrete type:

| Adapter | `ctx` argument | `GetQuery` returns | `GetSqlString` returns |
|---------|----------------|--------------------|------------------------|
| `RawAdapter{}` | table name `string` or `RawContext{Table}` | `SQLQuery{SQL, Args}` | SQL with literals interpolated (for display) |
| `GormAdapter{}` | `*gorm.DB` | `SQLQuery{SQL, Args}` | SQL via GORM DryRun, literals interpolated |
| `MongoAdapter{}` | `nil`, or `"AGG"` + joins for aggregation | `MongoFindQuery{Filter, Options}` / `MongoAggregateQuery{Pipeline, Options}` | `""` (no SQL form — use `GetQuery` or the `BuildMongo*` helpers) |
| `ElasticsearchAdapter{}` | `nil` | `ElasticsearchQueryWrapper{Query}` | the query as compact JSON |

Each adapter also has package-level helpers that skip the generic API: `AdapterRawGetSql`, `AdapterGormGetSql`, `AdapterMongoGetFind` / `AdapterMongoGetAggregate`, plus the `Build*` functions shown per adapter below.

### GORM adapter

```go
f := figo.New()
f.AddFiltersFromString(`status="active" and age>=18 sort=age:desc page=skip:0,take:20`)
f.Build(adapters.GormAdapter{})

// Option A: apply onto a *gorm.DB and execute yourself
var users []User
adapters.ApplyGorm(f, db.Model(&User{})).Find(&users)

// Option B: render the SQL string (DryRun) for logging/inspection
sql := f.GetSqlString(db.Model(&User{}))           // full SELECT
where := f.GetSqlString(db.Model(&User{}), "WHERE") // just the WHERE segment

// Option C: get placeholder SQL + args
q := f.GetQuery(db.Model(&User{})).(figo.SQLQuery)
// q.SQL, q.Args
```

`ApplyGorm` sets limit/offset, select fields, preloads, where clauses, and sort. A `*gorm.DB` that already carries a caller scope (e.g. a tenant filter `db.Where("org_id = ?", id)`) keeps that scope — figo's filters are applied **on top of** it, not instead of it.

### Raw SQL adapter

Dialect-aware: the zero value targets MySQL (backtick identifiers, `?` placeholders, `REGEXP`); set `Dialect` for PostgreSQL (`"col"`, `$1..$N`, `~`) or SQLite (`"col"`, `?`, `REGEXP`).

```go
f := figo.New()
f.AddFiltersFromString(`id=1 and name="test" sort=id:desc page=skip:0,take:20`)
f.Build(adapters.RawAdapter{})                                 // MySQL (default)

// PostgreSQL rendering:
f.Build(adapters.RawAdapter{Dialect: adapters.PostgresDialect})
q := f.GetQuery(adapters.RawContext{Table: "users"}).(figo.SQLQuery)
// q.SQL: SELECT * FROM "users" WHERE ("id" = $1 AND "name" = $2) ORDER BY "id" DESC LIMIT 20

// Custom variant (e.g. case-insensitive Postgres regex):
pg := *adapters.PostgresDialect
pg.RegexOperator = "~*"
f.Build(adapters.RawAdapter{Dialect: &pg})

f.Build(adapters.RawAdapter{})

// Full SELECT
sql, args := adapters.BuildRawSelect(f, "users")
// sql:  "SELECT * FROM `users` WHERE (`id` = ? AND `name` = ?) ORDER BY `id` DESC LIMIT 20"
// args: []any{int64(1), "test"}

// Explicit column list (used when no AddSelectFields were set)
sql, args = adapters.BuildRawSelect(f, "users", "id", "name")

// Just the WHERE fragment
where, whereArgs := adapters.BuildRawWhere(f)

// Or via the generic API with a table name / RawContext
sql = f.GetSqlString("users")
sql = f.GetSqlString(adapters.RawContext{Table: "users"}, "SELECT", "FROM", "WHERE", "SORT")
q := f.GetQuery(adapters.RawContext{Table: "users"}).(figo.SQLQuery) // q.SQL + q.Args
```

With no `conditionType` arguments you get the full SELECT; otherwise only the named segments are emitted, in the order you list them. Recognized segment keywords (case-insensitive): `SELECT`, `FROM`, `JOIN`, `WHERE`, `ORDER BY` / `SORT`, `LIMIT`, `OFFSET`, `PAGE` (LIMIT + OFFSET together).

Identifiers are quote-escaped per dialect — embedded quote runes are doubled (values are always parameterized) — so field/table names can't break out of quoting. The `Build*` helpers (`BuildRawWhere`, `BuildRawSelect`, `BuildRawPreloads`) pick up the dialect from the instance's adapter, including `$N` numbering on Postgres.

`load=` preloads render into the full SELECT as `JOIN <table> ON <filter>` clauses (deterministic table order). If you'd rather run your own join/second-query logic, the same preload filters are exposed as rendered `WHERE` fragments:

```go
f.AddFiltersFromString(`id>0 load=[Orders:total>100]`)
f.Build(adapters.RawAdapter{})
preloads := adapters.BuildRawPreloads(f)          // map[string]RawPreload
// preloads["Orders"] == RawPreload{Where: "`total` > ?", Args: []any{int64(100)}}
```

### MongoDB adapter

```go
f := figo.New()
f.AddFiltersFromString(`status="active" and age>=18 sort=age:desc page=skip:0,take:20`)
f.Build(adapters.MongoAdapter{})

// Filter + find options directly
filter, err := adapters.BuildMongoFilter(f)   // bson.M
opts := adapters.BuildMongoFindOptions(f)      // *options.FindOptions (sort/limit/skip)

// Or via the generic API — returns MongoFindQuery
q := f.GetQuery(nil).(adapters.MongoFindQuery) // q.Filter, q.Options

// Aggregation pipeline with joins ($lookup) — pass "AGG" and a joins map
joins := map[string]adapters.MongoJoin{
	"orders": {From: "orders", LocalField: "_id", ForeignField: "user_id", As: "orders"},
}
pipeline, err := adapters.BuildMongoAggregatePipeline(f, joins)
```

`$in`/`$nin` always receive a real array (never `null`), so empty-list filters don't error at the server.

### Elasticsearch adapter

```go
f := figo.New()
f.AddFiltersFromString(`name=^"%john%" and age>=18 sort=age:desc page=skip:0,take:20`)
f.AddSelectFields("id", "name", "age")
f.Build(adapters.ElasticsearchAdapter{})

query, err := adapters.BuildElasticsearchQuery(f) // adapters.ElasticsearchQuery (Query/Sort/From/Size/Source)

// Or via the generic API — returns ElasticsearchQueryWrapper
q := f.GetQuery(nil).(adapters.ElasticsearchQueryWrapper) // q.Query is the ElasticsearchQuery
_ = q.GetSQL()  // the query as compact JSON (GetArgs() is nil — ES has no bind params)

// JSON string forms
jsonStr, err := adapters.GetElasticsearchQueryString(f)         // pretty
compact, err := adapters.GetElasticsearchQueryStringCompact(f)  // compact

// Fluent builder
esq := adapters.NewElasticsearchQueryBuilder().
	FromFigo(f).
	AddSort("name", true).
	SetPagination(0, 10).
	SetSource("id", "name").
	Build()

// The builder can also emit JSON directly
jsonStr, err = adapters.NewElasticsearchQueryBuilder().FromFigo(f).ToJSON()  // or ToJSONCompact()
```

`AddSelectFields` maps to `_source`, `page=` maps to `from`/`size`, `sort=` maps to the ES sort array. `ElasticsearchQuery` is JSON-ready (`Query`, `Sort`, `From`, `Size`, `Source` fields with the right tags), so you can marshal it straight into a search request body. If the built AST contains an expression the ES adapter can't render, `BuildElasticsearchQuery` returns the error; the fluent builder's `FromFigo` (which has no error return) defers it until `ToJSON`/`ToJSONCompact`.

### Writing your own adapter

An adapter is anything implementing the two-method `figo.Adapter` interface, so you can target another backend (or another SQL dialect) yourself:

```go
type Adapter interface {
	GetSqlString(f Figo, ctx any, conditionType ...string) (string, bool)
	GetQuery(f Figo, ctx any, conditionType ...string) (Query, bool)
}
```

Walk `f.GetClauses()` / `f.GetPreloads()` (the `Expr` AST), honor `f.GetPage()`, `f.GetSort()`, and `f.GetSelectFields()`, and return a query value. `figo.Query` is a marker interface with an exported method, so your adapter can define its own typed result:

```go
type MyQuery struct{ /* ... */ }

func (MyQuery) IsQuery() {}
```

(`figo.SQLQuery` fits most custom SQL dialects if you'd rather reuse it.) The core exposes the AST utilities adapters and filters need — `figo.ExprField`, `figo.PruneExprFields`, `figo.CloneExpr`, `figo.NodeField`/`figo.SetNodeField` — and the `adapters` package is itself the reference implementation. Pass an instance to `Build(myAdapter)` and the generic `GetSqlString` / `GetQuery` API routes through it.

## The `Figo` API

`figo.New()` returns the `Figo` interface. The most commonly used methods:

**Filters & building**

```go
AddFiltersFromString(dsl string) error
AddFilter(exp Expr)                 // add a programmatic AST node
Build(adapter Adapter)              // pass nil to rebuild with the current adapter
GetClauses() []Expr
GetPreloads() map[string][]Expr
GetDSL() string
```

**Rendering**

```go
GetSqlString(ctx any, conditionType ...string) string
GetQuery(ctx any, conditionType ...string) Query
```

Cached rendering lives on the `CachePlugin` (see [Caching](#caching)): `cp.GetCachedSqlString(f, ctx)` / `cp.GetCachedQuery(f, ctx)`.

`GetQuery` gives placeholder SQL + args for execution; `GetSqlString` on the SQL adapters interpolates literals, making it the display/logging form. For a view of the parsed AST itself, use `Explain()`.

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

**Plugins**

```go
RegisterPlugin(plugin Plugin) error // Initialize is called; rolled back if it errors
UnregisterPlugin(name string) error
SetPluginManager(m *PluginManager)
GetPluginManager() *PluginManager
```

**Field & select control** — `AddSelectFields(...)` / `GetSelectFields()` (`map[string]bool`), `SetNamingFunc(fn)` / `GetNamingFunc()`. Ignore/whitelist state lives on the `FieldsPlugin`, complexity limits on the `LimitsPlugin`.

> `GetPage()` returns a **copy** of the page. Mutating it has no effect — call `SetPage(skip, take)` to change pagination.

## Field safety: ignore lists & whitelist

Because DSL usually comes from untrusted input, figo gives you two ways to constrain which fields a caller may filter on — both live on the `FieldsPlugin`. Register it **before** adding filters. Both prune the built AST — dropping a condition never leaves a dangling `and`/`or`/`not` behind, and both apply to DSL filters and to programmatic `AddFilter` clauses alike (the plugin implements the `ExprFilter` hook, which `Build` and `AddFilter` run on every expression entering the clause tree).

**Ignore list** — silently drop specific fields:

```go
f := figo.New()
fp := plugins.NewFieldsPlugin()
fp.AddIgnoreFields("password", "internal_notes")
f.RegisterPlugin(fp)

f.AddFiltersFromString(`name="x" and password="y"`)
f.Build(adapters.RawAdapter{})
// only name survives
```

Ignore names match both the raw and naming-converted spelling, so `AddIgnoreFields("user_name")` also blocks `userName` under the snake_case strategy.

**Whitelist** — allow *only* listed fields:

```go
fp := plugins.NewFieldsPlugin()
fp.SetAllowedFields("name", "age", "status")
fp.EnableFieldWhitelist()
f.RegisterPlugin(fp)

f.AddFiltersFromString(`name="x" and secret="y"`) // secret is dropped
f.Build(adapters.RawAdapter{})

// Also on the plugin: DisableFieldWhitelist(), IsFieldAllowed(field), IsFieldWhitelistEnabled(), GetAllowedFields()
```

**Select fields** — restrict returned columns (SQL `SELECT`, ES `_source`). Selects are projection state consumed by the adapters, so they stay on the instance itself:

```go
f.AddSelectFields("id", "name", "email")
```

## Query complexity limits

`LimitsPlugin` guards against pathological untrusted DSL: once registered, every `AddFiltersFromString` call is measured and fails when a limit is exceeded. A zero value disables that particular limit.

```go
lp := plugins.NewLimitsPlugin(plugins.DefaultQueryLimits()) // nesting 10, fields 50, params 100, expressions 200
f.RegisterPlugin(lp)

err := f.AddFiltersFromString(hugeUntrustedDSL) // e.g. "query exceeds MaxParameterCount: 250 > 100"
```

Limits are measured after any registered field pruning, i.e. on the query that would actually run.

## Mandatory scopes (multi-tenant)

`ScopePlugin` guarantees that server-side filters are present in **every** built query — the row-level-security pattern for multi-tenant apps. The scope is injected at the end of every `Build`, including a build with no filters at all, so an unfiltered query cannot escape it; it's injected after whitelist pruning, so callers can't strip it either.

```go
sp := plugins.NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: tenantID})
f.RegisterPlugin(sp)

f.AddFiltersFromString(untrustedDSL) // whatever the caller sends...
f.Build(adapters.RawAdapter{})
// ...the rendered WHERE always includes AND `tenant_id` = ?
```

Multiple scopes are ANDed in; `sp.AddScope(...)` adds more. Rebuilds never duplicate an already-present scope.

## Auditing

`AuditPlugin` records every parsed DSL and every rendered statement — for compliance logs and "what did it actually run?" debugging. Entries go to an optional `log/slog` logger and a bounded in-memory history (on cached paths, only real renders are recorded — not cache hits).

```go
ap := plugins.NewAuditPlugin(slog.Default(), 100) // nil logger = history only; size 0 = log only
f.RegisterPlugin(ap)

// ... parse and render ...
for _, e := range ap.History() { // oldest first
	fmt.Println(e.At, e.Kind, e.DSL, e.Result)
}
```

## Naming

By default figo converts DSL field names to `snake_case` (so `userName` → `user_name`). Naming is a single `NamingFunc` — pick a built-in or supply your own; it applies to the DSL and every adapter's column normalization (they never disagree).

```go
f.SetNamingFunc(figo.NoChangeNaming)  // keep names verbatim
f.SetNamingFunc(figo.SnakeCaseNaming) // default

// Or any custom function:
f.SetNamingFunc(func(field string) string {
	return "t_" + field
})

f.SetNamingFunc(nil) // reset to the default (SnakeCaseNaming)
```

The regex SQL operator for `=~`/`!=~` has a per-adapter home:

- **Raw adapter**: comes from the dialect — `REGEXP` on MySQL/SQLite, `~` on Postgres. Customize by copying a dialect (`pg := *adapters.PostgresDialect; pg.RegexOperator = "~*"`).
- **GORM adapter**: uses the **package-level** setting (process-wide, safe to change concurrently):

```go
figo.SetRegexSQLOperator("REGEXP") // MySQL / SQLite (default)
figo.SetRegexSQLOperator("~")      // PostgreSQL, case-sensitive
figo.SetRegexSQLOperator("~*")     // PostgreSQL, case-insensitive
op := figo.GetRegexSQLOperator()
```

## Inspecting & transforming the AST

**`Explain()`** renders the parsed AST as an indented tree — handy for debugging precedence/grouping without a database:

```go
f.AddFiltersFromString(`id=1 and (age>20 or active=true)`)
f.Build(adapters.RawAdapter{})
fmt.Println(f.Explain())
// AND
//  ├── id = 1
//  └── OR
//      ├── age > 20
//      └── active = true
```

Strings are quoted in the output so `name = "john"` is visually distinct from `active = true`; multiple top-level clauses appear under a synthetic `AND` root (matching how the builders combine them); an unparsed instance yields `(no filters)`.

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

A package-level `figo.Walk(expr, visit)` is also available for traversing a standalone `Expr` tree (it returns the rewritten expression).

## Caching

Caching ships as a plugin, not core figo state: a `CachePlugin` caches rendered SQL/query results keyed by the full instance state (DSL, clauses, page, sort, field sets, naming, adapter type, regex operator, context). One plugin can serve many `Figo` instances.

```go
cp := plugins.NewCachePlugin(plugins.CacheConfig{
	Enabled:         true,
	TTL:             5 * time.Minute,
	MaxSize:         1000,          // 0 = unlimited; LRU eviction at capacity
	CleanupInterval: time.Minute,   // background expiry sweep
})

sql := cp.GetCachedSqlString(f, adapters.RawContext{Table: "users"})
q := cp.GetCachedQuery(f, adapters.RawContext{Table: "users"})

stats := cp.Stats() // hits, misses, size, hit rate
cp.Clear()
cp.Close()          // stops the owned cache's cleanup goroutine

// Or inject your own implementation of the QueryCache interface:
cp.SetCache(myCache)   // GetCache() / GetConfig() read the current state
```

Cache keys are type-aware — `a = int64(1)` and `a = "1"` never collide even when instances share a cache. A cache the plugin created itself stops its background goroutine when it's replaced, `Close`d, or when the plugin is garbage-collected. `NewInMemoryCache(config)` is available if you want to manage one directly (call `Stop()` when done).

## Performance monitoring

Monitoring ships as a plugin, not core figo state: a `MetricsPlugin` wraps a `PerformanceMonitor` you attach to whatever produces metrics — typically the `CachePlugin`.

```go
mp := plugins.NewMetricsPlugin(true)

cp := plugins.NewCachePlugin(plugins.CacheConfig{Enabled: true, TTL: time.Minute})
cp.SetPerformanceMonitor(mp.PerformanceMonitor)

// ... render through cp ...

m := mp.GetMetrics()
// m.QueryCount, m.CacheHits, m.CacheMisses, m.AverageLatency, m.ErrorCount, ...
mp.Reset()
```

The cache plugin's `GetCachedSqlString` / `GetCachedQuery` record latency and hit/miss outcomes into the attached monitor. A bare `plugins.NewPerformanceMonitor(true)` works too, and you can record manually via `mon.RecordQuery(latency, cacheHit, err)`.

## Plugins

Register plugins to hook into the parse, build, and render pipelines. Each plugin implements `Name`, `Version`, `Initialize`, `BeforeParse`, `AfterParse`, `BeforeQuery`, `AfterQuery` — and may optionally implement the `ExprFilter` hook (per-expression transform/prune; see [Field safety](#field-safety-ignore-lists--whitelist)) and/or the `ClauseFinalizer` hook (whole-clause-list transform at the end of every `Build`; see [Mandatory scopes](#mandatory-scopes-multi-tenant)).

```go
f.RegisterPlugin(myPlugin)   // Initialize is called; rolled back if it errors
f.UnregisterPlugin("my-plugin")
```

All hooks fire automatically:

- `BeforeParse` / `AfterParse` inside `AddFiltersFromString` (`BeforeParse` can rewrite the DSL; an `AfterParse` error fails the call).
- `FilterExpr` (optional) on every expression entering the clause tree — `Build` and `AddFilter` alike.
- `FinalizeClauses` (optional) once at the end of every `Build`, even one with no filters.
- `BeforeQuery` / `AfterQuery` around every `GetSqlString` / `GetQuery` render. A `BeforeQuery` error vetoes the render (`""` / `nil` is returned); an `AfterQuery` error vetoes the rendered result the same way — useful for authorization, auditing, and logging. On the cached paths (`CachePlugin.GetCached*`) they fire on misses, when a render actually happens, not on hits.

Hooks run on a snapshot outside the manager's lock, so a hook may call back into the manager without deadlocking. Query hooks must not render through the same instance (that would recurse).

**Eight built-in plugins** cover the common policies — each documented in its own section above: [`SyntaxPlugin`](#input-validation--repair), [`FieldsPlugin`](#field-safety-ignore-lists--whitelist), [`LimitsPlugin`](#query-complexity-limits), [`ValidationPlugin`](#validation), [`ScopePlugin`](#mandatory-scopes-multi-tenant), [`CachePlugin`](#caching), [`MetricsPlugin`](#performance-monitoring), [`AuditPlugin`](#auditing).

See [PLUGIN_SYSTEM_GUIDE.md](PLUGIN_SYSTEM_GUIDE.md) for a full walkthrough with example plugins.

## Validation

Validation ships as a plugin, not core figo state: create a `ValidationPlugin`, attach rules to fields, and register it on the instance. Built-in validators include `required`, `min_length`, and `email`.

```go
vp := plugins.NewValidationPlugin()
vp.RegisterValidator(plugins.EmailValidator{})
vp.AddRule(plugins.ValidationRule{Field: "email", Rule: "email", Message: "invalid email"})
f.RegisterPlugin(vp)

// Once registered, every AddFiltersFromString call is validated via the
// AfterParse hook — parsing fails on the first rule violation:
err := f.AddFiltersFromString(`email="not-an-email"`) // error

// One-off checks work directly on the plugin:
if err := vp.Validate("email", "not-an-email"); err != nil {
	// handle
}
```

Rule handlers run on a snapshot (a handler may safely call back into the plugin). The former core validation manager (`NewValidationManager`, `SetValidationManager`, `AddValidationRule`, `RegisterValidator`, `ValidateField` on `Figo`) has been removed in favor of this plugin.

## Input validation & repair

`AddFiltersFromString` stores input as-is. Syntax validation (and optional auto-repair) ships as a plugin: register a `SyntaxPlugin` and its `BeforeParse` hook validates every DSL before parsing.

```go
// Strict: reject malformed DSL with a structured error
f.RegisterPlugin(plugins.NewSyntaxPlugin(false))
err := f.AddFiltersFromString(`name = = 5`) // *figo.ParseError (wrapped)

// Repair mode: fix common malformations first, validate what remains
f2 := figo.New()
f2.RegisterPlugin(plugins.NewSyntaxPlugin(true))
err = f2.AddFiltersFromString(`(name="john" and age>25`) // adds missing ')'

// Structured errors
var perr *figo.ParseError
if errors.As(err, &perr) {
	fmt.Printf("%s at line %d col %d\n", perr.Message, perr.Line, perr.Column)
}
```

Repairs cover unmatched parentheses/quotes/brackets and dangling trailing/leading `and`/`or`. A leading `not` is **not** treated as malformed and is never stripped. Repair means querying something other than what the caller literally sent — enable it deliberately.

## Concurrency

A `Figo` instance is guarded by an internal `sync.RWMutex`, and the ancillary collaborators (the plugin manager and each built-in plugin) carry their own locks. Read-render methods (`GetSqlString`, `GetQuery`, the cache plugin's `GetCached*`) are safe to call concurrently after `Build`, plugin hooks and expression filters run outside the instance lock (so they may call back into read methods), and the package is race-clean under `go test -race`.

For concurrent **writers** — multiple goroutines calling `AddFiltersFromString`/`Build` on the *same* instance — prefer giving each goroutine its own instance (or a `Clone()`), since those mutate shared builder state. The safe, common pattern:

```go
base := figo.New()
base.AddFiltersFromString(`status="active"`)
base.Build(adapters.RawAdapter{})

var wg sync.WaitGroup
for i := 0; i < 10; i++ {
	wg.Add(1)
	go func() {
		defer wg.Done()
		// concurrent reads on the same built instance are safe
		_ = base.GetSqlString(adapters.RawContext{Table: "users"})
	}()
}
wg.Wait()
```

## Testing

```bash
go test ./...            # unit + adapter tests
go test -race ./...      # race detector
```

No live databases are needed: the MongoDB adapter tests use the BSON encoder directly, the Elasticsearch adapter tests assert on the generated query JSON, and the GORM tests run against in-memory SQLite. Tests are split across the three packages — core behavior in the root (`figo_test`), adapter rendering in `adapters/`, plugin behavior and integration in `plugins/`.

Runnable usage examples live in [examples/example_usage.go](examples/example_usage.go).

## Status of features

Fully wired end-to-end: the DSL and all operators above, the four adapters (raw SQL with MySQL/PostgreSQL/SQLite dialects), select-field control, naming funcs, pagination/sort/preloads, the `Explain`/`Clone`/`Walk` AST tools, the full plugin hook surface (parse, expression-filter, clause-finalizer, and query hooks), and the eight built-in plugins: `SyntaxPlugin` (validation & repair), `FieldsPlugin` (ignore/whitelist), `LimitsPlugin` (complexity limits), `ValidationPlugin` (value rules), `ScopePlugin` (mandatory filters), `CachePlugin`, `MetricsPlugin`, and `AuditPlugin`.

Partial / not yet wired (defined in the API but not auto-invoked or without adapter support):

- Advanced expression types (`JsonPathExpr`, `ArrayContainsExpr`, `ArrayOverlapsExpr`, `FullTextSearchExpr`, `GeoDistanceExpr`, `CustomExpr`) — defined for programmatic `AddFilter`, but adapters return an "unsupported expression" error for them rather than rendering.

## Contributing

Pull requests welcome:

1. Branch off `main` (don't PR against `main` directly from `main`).
2. Include tests covering your change; keep `go test -race ./...` green.
3. Update this README when you change behavior or the API.

## License

BSD 2-Clause. See [LICENSE](LICENSE).
