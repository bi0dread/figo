# figo

Build GORM, SQL, MongoDB, and Elasticsearch queries from a filter string:

```text
status="active" and age>=18 sort=age:desc page=skip:0,take:20
```

**Go 1.23+ · v4 · BSD 2-Clause**

[Quick start](#quick-start) · [DSL](#dsl) · [Adapters](#adapters) · [Plugins](#plugins) · [Programmatic filters](#programmatic-filters) · [Development](#development)

## Install

```sh
go get github.com/bi0dread/figo/v4
```

Use `figo` for the builder, `figo/adapters` for backend output, and `figo/plugins` for optional policies.

## Quick start

This example generates parameterized PostgreSQL without a database connection:

```go
package main

import (
	"fmt"
	"log"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
)

func main() {
	f := figo.New()
	if err := f.AddFiltersFromString(`status="active" and age>=18 sort=age:desc page=skip:0,take:20`); err != nil {
		log.Fatal(err)
	}
	if err := f.BuildE(adapters.RawAdapter{Dialect: adapters.PostgresDialect}); err != nil {
		log.Fatal(err)
	}

	query, args, err := adapters.BuildRawSelect(f, "users", "id", "name")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(query)
	fmt.Println(args)
}
```

```text
SELECT "id", "name" FROM "users" WHERE ("status" = $1 AND "age" >= $2) ORDER BY "age" DESC LIMIT 20
[active 18]
```

Execute the query with its arguments through your database driver. Use `GetSqlString` on SQL adapters for display or logging; use bound parameters for execution.

The workflow is **configure → add filters → build → render**:

- `AddFiltersFromString` replaces the stored DSL and runs parse hooks. Check its error.
- `BuildE(adapter)` parses the DSL and applies build policies. Reject a non-nil error for user input: the partial query may omit a requested condition. `Build` discards these diagnostics.
- Render through an adapter helper and check its error too. Parse success does not guarantee that a backend supports the query.

`figo.New()` takes no adapter. `BuildE(nil)` rebuilds with the current adapter. Use a fresh builder per request; concurrent rendering is supported after you finish configuring and building it.

## DSL

Combine conditions with `and`, `or`, `not`, and parentheses:

```text
name="John Doe" and (age>=18 or vip=true) and status<nin>["banned","deleted"]
```

Precedence is `not` → `and` → `or`. Adjacent conditions imply `and`.

| Operation | Syntax | Example |
|---|---|---|
| Equal / not equal | `=` / `!=` | `status="active"` |
| Compare | `>` / `>=` / `<` / `<=` | `age>=18` |
| In / not in | `<in>` / `<nin>` | `id<in>[1,2,3]` |
| Inclusive range | `<bet>` | `price<bet>(10..100)` |
| Null / not null | `<null>` / `<notnull>` | `deleted_at<null>` |
| LIKE / NOT LIKE | `=^` / `!=^` | `name=^"%john%"` |
| Case-insensitive LIKE | `.=^` | `name.=^"%john%"` |
| Regex / negated regex | `=~` / `!=~` | `name=~"^Jo"` |

LIKE uses `%` for any sequence and `_` for one character. Regex behavior depends on the backend. MongoDB screens some expensive patterns, but the screen does not bound execution cost; restrict public regex input and set database timeouts.

### Values

Unquoted numbers, booleans, `null`, and recognized dates become typed Go values. Other unquoted values become strings. Double quotes preserve strings: `code="0123"`, `flag="true"`, `date="2026-11-08"`.

- Use quotes for spaces or operator characters. The DSL has no escape syntax for an embedded double quote; pass such values through `AddFilter`.
- `x=null` and `x!=null` mean null and not-null checks.
- `<in>[]` matches nothing; `<nin>[]` matches everything. Blank list entries are skipped.
- `<bet>` requires two compatible bounds. Use `>=` or `<=` for an open-ended range.
- Use ISO dates to avoid ambiguous slash formats. `figo.ParseValue` exposes the same literal conversion.

### Sort, page, and preload

| Directive | Example |
|---|---|
| Sort, in priority order | `sort=created_at:desc,id:asc` |
| Offset and limit | `page=skip:20,take:10` |
| Related records with their own filters | `load=[Orders:total>100]` |
| Multiple relations | `load=[Orders:total>100 \| Profile:active=true]` |

**Pagination defaults to `skip:0,take:0`.** On SQL and MongoDB, a non-positive take means no limit. Enforce a maximum page size after `BuildE`; a client can override a limit you set before parsing. `LimitsPlugin` limits query complexity, not returned rows.

Nested preload paths such as `Orders.Items` are disabled by default. Keep them disabled for untrusted queries that depend on tenant scopes: combining nested paths with field pruning can bypass an intermediate relation's scope. Parent scopes do not scope child records; see [Plugins](#plugins).

## Adapters

Pass one of these to `BuildE`:

| Backend | Adapter | Render or execute |
|---|---|---|
| MySQL | `adapters.RawAdapter{}` | `BuildRawSelect` / `BuildRawWhere` |
| PostgreSQL | `adapters.RawAdapter{Dialect: adapters.PostgresDialect}` | Same helpers, `$1` placeholders |
| SQLite | `adapters.RawAdapter{Dialect: adapters.SQLiteDialect}` | Same helpers, `?` placeholders |
| GORM | `adapters.GormAdapter{}` | `ApplyGorm` |
| MongoDB | `adapters.MongoAdapter{}` | `AdapterMongoGetFind` / `BuildMongoAggregatePipeline` |
| Elasticsearch | `adapters.ElasticsearchAdapter{}` | `BuildElasticsearchQuery` |

### GORM

After building with `GormAdapter`, apply the query to your model:

```go
var users []User
if err := adapters.ApplyGorm(f, db.Model(&User{})).Find(&users).Error; err != nil {
	return err
}
```

`ApplyGorm` preserves caller filters and adds Figo's conditions, projection, sort, page, and preloads. Check the returned DB's `Error`. Use a fresh base handle when rebuilding a Figo instance; reusing a handle that already carries its applied state can retain stale filters.

### Raw SQL

`BuildRawSelect(f, table, columns...)` returns SQL, arguments, and an error. `BuildRawWhere(f)` returns the condition fragment and arguments. `SetSelectFields` takes precedence over the helper's column arguments; no projection means `SELECT *`.

`load=` contributes nothing to the raw adapter's SELECT. Use `BuildRawPreloads(f)` to get each relation's `Where` and `Args`, then supply your own join keys or child queries.

### MongoDB

For a find query, get the filter and options together:

```go
filter, opts, err := adapters.AdapterMongoGetFind(f)
if err != nil {
	return err
}
// Pass filter and opts to collection.Find.
```

For `load=[Orders:total>100]`, provide the relation mapping:

```go
joins := map[string]adapters.MongoJoin{
	"Orders": {From: "orders", LocalField: "_id", ForeignField: "user_id", As: "orders"},
}
pipeline, err := adapters.BuildMongoAggregatePipeline(f, joins)
if err != nil {
	return err
}
// Pass pipeline to collection.Aggregate.
```

Join-map keys must match the DSL relation label, including case. `BuildMongoAggregatePipeline` returns an error for missing mappings. Use scalar join keys, and keep lookup aliases separate from root fields used by mandatory scopes: a lookup can overwrite those fields before the scope runs.

The adapter converts valid 24-character hex strings on `_id` to ObjectIDs. Configure other fields with `MongoAdapter{ObjectIDFields: []string{"_id", "user_id"}}`; an explicit empty slice disables conversion.

### Elasticsearch

`BuildElasticsearchQuery` returns a JSON-ready query and an error. `SetSelectFields` controls `_source`; sorting and pagination map to `sort`, `from`, and `size`. Use `GetElasticsearchQueryString` or `GetElasticsearchQueryStringCompact` for JSON output.

On adapter render errors, `ElasticsearchAdapter.GetQuery` / `GetSqlString` return `match_none` with `ok=true`. Checking `if q == nil` or `if !ok` will therefore never fire. Use `BuildElasticsearchQuery` to detect these errors; query-hook vetoes through `f.GetQuery` / `f.GetSqlString` can still return nil or an empty string.

For `take:0`, the adapter uses `max(0, max_result_window - from)`, assuming a 10,000-result window. Explicit positive limits pass through unchanged. Keep pagination within that window; deeper pagination needs a separate `search_after` or scroll implementation.

### Backend differences

- **Preloads:** GORM fetches children without removing parents. MongoDB filters the joined array and removes parents with no matching child when a child filter exists. Raw SQL exposes child predicates for you to execute. Elasticsearch rejects preloads.
- **Nulls:** SQL nulls and document-store null/missing fields differ, especially under negation and list membership. Use explicit `<null>` / `<notnull>` checks where the distinction matters.
- **Advanced expressions:** JSON paths, array containment/overlap, full-text search, and geo distance are programmatic features for MongoDB and Elasticsearch. SQL adapters return unsupported-expression errors.
- **Custom SQL:** `CustomExpr` works on raw SQL and GORM. Its handler owns identifier quoting and must return SQL with bind arguments. Document adapters reject it.

## Plugins

Register policies **before** `AddFiltersFromString`. Import `github.com/bi0dread/figo/v4/plugins`.

| Plugin | Use it to |
|---|---|
| `InjectionGuardPlugin` | Reject invalid identifiers; restrict DSL fields with `AllowFields` |
| `FieldsPlugin` | Prune ignored or non-whitelisted fields from filters, sort, and explicit projections |
| `LimitsPlugin` | Bound nesting, fields, parameters, expressions, and sort columns |
| `ValidationPlugin` | Validate field values with rules or custom handlers |
| `ScopePlugin` | Add mandatory root conditions at build time; configure child scopes separately |
| `SyntaxPlugin` | Check syntax; enable input repair only when intended |
| `CachePlugin` | Cache rendered queries, not database results |
| `MetricsPlugin` | Record measurements through its monitor, including attached cache activity |
| `AuditPlugin` | Record parse attempts and rendered queries |

For a tenant-scoped query, register the scope using an identity from your application:

```go
scope := plugins.NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: tenantID})
scope.AddPreloadScope("Orders", figo.EqExpr{Field: "tenant_id", Value: tenantID})

for _, p := range []figo.Plugin{
	plugins.NewInjectionGuardPlugin(),
	plugins.NewLimitsPlugin(plugins.DefaultQueryLimits()),
	scope,
} {
	if err := f.RegisterPlugin(p); err != nil {
		return err
	}
}
// Now call AddFiltersFromString, BuildE, and the adapter helper.
```

`AddPreloadScope` matches exact relation labels. Use `AddPreloadScopeAll` only when the same condition fits each child model. Keep the nested-preload and MongoDB alias limitations above in mind.

Default complexity limits: nesting **10**, filter fields **50**, parameters **100**, expressions **200**, sort columns **32**. Zero or negative values disable a limit. Measurement includes preload filters and runs after field pruning, excluding injected scopes.

`FieldsPlugin` drops disallowed conditions; use `InjectionGuardPlugin.AllowFields` when you want rejection instead. Set returned fields with `f.SetSelectFields("id", "name")`: registering a field policy alone does not replace the default `SELECT *`.

Query hooks wrap `f.GetQuery` / `f.GetSqlString`, not direct adapter helpers. Cache hits skip those hooks, so keep per-request authorization outside the cache. GORM contexts bypass caching. Treat cached MongoDB/Elasticsearch results as read-only, and close owned cache plugins when finished.

`GetPluginManager()` is never nil. Use `ListPlugins()` to inspect registrations. See the [plugin guide](PLUGIN_SYSTEM_GUIDE.md) for hook interfaces and configuration examples.

## Programmatic filters

Use typed Go values with `AddFilter`:

```go
f := figo.New()
f.AddFilter(figo.EqExpr{Field: "status", Value: "active"})
f.AddFilter(figo.InExpr{Field: "role", Values: []any{"admin", "editor"}})
if err := f.BuildE(adapters.RawAdapter{}); err != nil {
	return err
}
```

Top-level expressions combine with AND. Use `AndExpr`, `OrExpr`, and `NotExpr` to group them; a multi-operand `NotExpr` means `NOT (a OR b)`.

**Mixing DSL and Go expressions:** add programmatic filters after `BuildE`. Rebuilding a non-empty DSL replaces them. Put mandatory conditions in `ScopePlugin` so they survive rebuilds.

| Task | API |
|---|---|
| Preserve field spelling | `f.SetNamingFunc(figo.NoChangeNaming)` before adding filters |
| Select returned fields | `f.SetSelectFields("id", "name")` before building |
| Set pagination | `f.SetPage(skip, take)`; apply server caps after building |
| Inspect the AST | `f.Explain()` / `f.GetClauses()` / `f.GetPreloads()` |
| Copy query state | `f.Clone()`; plugins and adapters remain shared |
| Rewrite expressions | `f.Walk(...)` with `figo.NodeField` / `figo.SetNodeField` |

Default naming converts `userName` to `user_name` and preserves dotted paths. Pass original field names to `AddFilter` and `SetSort`; pre-converting can apply a custom naming function twice.

Implement `figo.Adapter` for another backend, or `figo.Plugin` and its optional hooks for another policy. Inspect the exported interfaces with `go doc github.com/bi0dread/figo/v4`.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

The tests use in-memory SQLite and MongoDB/Elasticsearch query assertions; no live database services are required.

- [Runnable Elasticsearch examples](examples/example_usage.go)
- [Plugin guide](PLUGIN_SYSTEM_GUIDE.md)
- [Live visual query builder](https://bi0dread.github.io/figo/) and [playground setup](playground/README.md)
- [BSD 2-Clause license](LICENSE)
