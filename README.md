# Go Gorm plugin Library (figo) v3

The figo package provides a robust mechanism for building dynamic filters for SQL queries in applications that use the GORM ORM library. It simplifies the process of defining filters through a domain-specific language (DSL) and converting them into GORM clauses, offering a powerful tool for creating flexible and complex queries.
## Differences from gorm package

just makes gorm clauses from string

### Package Name

figo


### Installation
``` bash
go get github.com/bi0dread/figo/v3
```

# Features
* DSL-Based Filter Parsing \
  Easily construct complex filters using a concise DSL format like:
```text
(id=1 and vendorId=22) and bank_id>11 or expedition_type="eq" load=[TestInner1:id=3 or name=test1 | TestInner2:id=4] sort=id:desc page=skip:0,take:10
```
* Rich Operations
  - Comparisons: =, !=, >, >=, <, <=
  - Like: =^"%ab%" (LIKE), .=^"%ab%" (ILIKE)
  - Regex: =~"^ab.*" and !=~"^ab.*"
  - Sets: <in>[1,2,3], <nin>[x,y]
  - Ranges: <bet>10..20 or <bet>(10..20)
  - Null checks: <null>, <notnull>
  - Logical: and, or, not
  - Sorting: sort=field:asc,other:desc
  - Pagination: page=skip:0,take:10
  - Preloads/Joins: load=[Rel1:..., Rel2:...]
* Multiple Adapters
  - GORM (builds clauses and explained SQL)
  - Raw SQL (portable SELECT/WHERE/ORDER/LIMIT rendering)
  - MongoDB (filters, find options, aggregation pipeline)

### GORM Integration
The figo package converts filters into ORM-agnostic expressions that adapters (GORM/Raw/Mongo) translate into executable queries.

### Pagination Support
Manage result limits and offsets with the page operation.

### Field Restrictions
Prevent specific fields from being included in queries.

# Usage
* Creating a Figo Instance 
```go
// Pass an adapter or nil (for building only)
f := figo.New(nil)                    // no adapter
f := figo.New(figo.GormAdapter{})     // GORM integration
f := figo.New(figo.RawAdapter{})      // Raw SQL string builder
f := figo.New(figo.MongoAdapter{})    // Mongo typed queries (no SQL)
```
* Adding Filters
```go
f.AddFiltersFromString("(id=1 and vendorId=22) and bank_id>11 or expedition_type=\"eq gg\" load=[TestInner1:id=3 or name=test1 | TestInner2:id=4] sort=id:desc page=skip:0,take:10")
```
if you want to set value with space char " " just put it in quotes  e.g expedition_type="eq gg"


* Manually

```go
f.AddFilter(figo.EqExpr{Field: "id", Value: 9})

```

* Restricting Fields\
  Prevent certain fields from being queried:
```go
f.AddIgnoreFields("sensitive_field", "internal_use_only")

```

* Building Filters\
  After adding filters, invoke Build to process the clauses:
```go
f.Build()
```
* Applying Filters to a GORM Query\
  Use the helper to apply filters to your GORM query:
```go
db = figo.ApplyGorm(f, db)
```
* Get query string\
  Ask the figo instance for a rendered SQL string via the selected adapter:
```go
// For GORM: pass the *gorm.DB (already configured with Model(...))
sqlQuery := f.GetSqlString(db, "SELECT", "FROM", "WHERE", "JOIN", "ORDER BY", "GROUP BY", "LIMIT", "OFFSET")

// For Raw: pass a table name or RawContext{Table: "..."}
rawQuery := f.GetSqlString(figo.RawContext{Table: "test_models"}, "SELECT", "FROM", "WHERE", "ORDER BY", "LIMIT", "OFFSET")

// For Mongo: Get typed queries instead of SQL
q := f.GetQuery(nil) // MongoFindQuery or MongoAggregateQuery
```

* Pagination\
  Control the result set's skip and take values via the DSL:
```go
f.AddFiltersFromString("page=skip:0,take:10")
```
 Or programmatically:
```go
f.GetPage().Skip = 10
f.GetPage().Take = 20
```
* Retrieving Preloads\
  If you’ve specified relationships to preload, retrieve them as follows:
```go
preloads := f.GetPreloads()
```

# Example: Full Workflow
```go
package main

import (
	"fmt"
	"github.com/bi0dread/figo/v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func main() {
	// Initialize GORM
	db, err := gorm.Open(sqlite.Open("test.db"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}

	// Create a Figo instance
	f := figo.New(figo.GormAdapter{})

	// Add filters from DSL
	f.AddFiltersFromString("(id=1 and vendorId=22) and bank_id>11 or expedition_type=\"eq\" load=[TestInner1:id=3 or name=test1 | TestInner2:id=4] sort=id:desc page=skip:0,take:10")

	// Add banned fields
	f.AddIgnoreFields("restricted_field")

	// Optionally select explicit fields
	f.AddSelectFields("id", "vendorId")
  
	// Build the filters
	f.Build()

	// Apply to GORM query
	db = figo.ApplyGorm(f, db)

	// Execute query
	var results []map[string]any
	db.Find(&results)

	fmt.Println("Query Results:", results)
}
```
# Domain-Specific Language (DSL) Syntax
* Syntax Format
```text
field operation value (field operation value field operation value(field operation value))...
```
## DSL Syntax

The DSL syntax allows you to define query filters dynamically:

- **Field Filters**: `field operation value` (id=3)
- **Logical Operations**: Combine filters using `and`, `or`, and `not`.
- **Sorting**: `sort=id:desc`
- **Pagination**: `page=skip:0,take:10`
- **Preloading Relations**: `load=[TestInner1:id=3 or name=test1 | TestInner2:id=4]`

* Examples
```text

Basic Filters

    id=10
    (Where id = 10)
    
Logical Operators

    id=10 or status=active
    (Where id = 10 OR status = 'active')
    
Sorting

    sort=name:asc,created_at=desc
    (Order by name ASC, created_at DESC)
    
Pagination

    page=skip:10,take:5
    (Skip 10 records and take 5)
    
Complex Filters

    (id=1 and vendorId=22) and bank_id>11 or expedition_type=eq load=[TestInner1:id=3 or name=test1 | TestInner2:id=4] sort=id:desc page=skip:0,take:10
```
## Supported Operations

| Operation | DSL Example                   | Description             |
|-----------|-------------------------------|-------------------------|
| `=`       | `field=value`                 | Equals                  |
| `>`       | `field>value`                 | Greater Than            |
| `>=`      | `field>=value`                | Greater Than or Equal   |
| `<`       | `field<value`                 | Less Than               |
| `<=`      | `field<=value`                | Less Than or Equal      |
| `!=`      | `field!=value`                | Not Equal               |
| `in`      | `not impl`                        | Value in List           |
| `notIn`   | `not impl`                        | Value not in List       |
| `like`    | `field=^"%val%"`                 | Like (Partial Match)    |
| `notLike` | `field!=^"%val%"`                | Not Like                |
| `between` | `not impl`                    | Between Range           |
| `and`     | `and`                         | Logical AND             |
| `or`      | `or`                          | Logical OR              |
| `not`     | `not`                         | Logical NOT             |

# Extensibility
* You can extend the package to support custom operations or additional parsing logic. Modify the operatorParser method for parsing custom DSL extensions.


## TODO
* improvement
* more operators

## Contributing

Pull requests are very much welcomed.  Create your pull request on a non-main
branch, make sure a test or example is included that covers your change, and
your commits represent coherent changes that include a reason for the change.

## License

BSD 2 clause, see LICENSE for more details.