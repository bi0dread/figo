package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type gormH8Company struct {
	ID    int
	Title string
}

type gormH8User struct {
	ID        int
	Age       int
	CompanyID int
	Company   gormH8Company
}

type gormH8Vault struct {
	ID    int
	Token string
}

func newGormH8DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&gormH8User{}, &gormH8Company{}, &gormH8Vault{}))
	require.NoError(t, db.Create(&gormH8Company{ID: 1, Title: "acme"}).Error)
	require.NoError(t, db.Create(&gormH8Vault{ID: 1, Token: "VAULT-TOKEN-XYZ"}).Error)
	// Two users share age 30 so DISTINCT is observable in the row COUNT.
	require.NoError(t, db.Create(&gormH8User{ID: 1, Age: 30, CompanyID: 1}).Error)
	require.NoError(t, db.Create(&gormH8User{ID: 2, Age: 30, CompanyID: 1}).Error)
	require.NoError(t, db.Create(&gormH8User{ID: 3, Age: 40, CompanyID: 1}).Error)
	return db
}

// G1: the figo projection must not pre-empt GORM's own SELECT construction.
//
// H4 closed a SQL injection by building a standalone clause.Select instead of
// feeding Statement.Selects. That clause is the one callbacks.BuildQuerySQL
// builds itself, carrying Statement.Distinct AND the columns appended for each
// Statement.Joins relation — so installing it made GORM's
// AddClauseIfNotExists a no-op and silently discarded both: a caller's
// Distinct() returned DUPLICATE ROWS (err=nil), and a caller's Joins("Rel")
// kept the JOIN but scanned the relation back ZERO-VALUED (err=nil), so
// application code read a real association as absent.
//
// Both properties are asserted here together with H4's own payload, because the
// fix is only correct if the projection is still injection-safe.
func TestG1_ProjectionKeepsCallerDistinctAndJoins(t *testing.T) {
	payload := "(SELECT/**/token/**/FROM/**/gorm_h8_vaults)AS/**/secret"

	newProjected := func(t *testing.T) Figo {
		t.Helper()
		f := New()
		f.SetSelectFields("age")
		f.Build(GormAdapter{})
		return f
	}

	t.Run("Distinct survives", func(t *testing.T) {
		db := newGormH8DB(t)
		f := newProjected(t)

		sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormH8User{}).Distinct())
		require.True(t, ok)
		t.Logf("sql=%q", sql)
		assert.Contains(t, sql, "SELECT DISTINCT", "the caller's Distinct() was dropped from the projected SELECT: %q", sql)

		var rows []map[string]any
		require.NoError(t, ApplyGorm(f, db.Model(&gormH8User{}).Distinct()).Find(&rows).Error)
		t.Logf("rows=%v", rows)
		assert.Len(t, rows, 2, "DISTINCT was dropped, so a de-duplicating query returned duplicates: %v", rows)
	})

	t.Run("Distinct(column) survives", func(t *testing.T) {
		db := newGormH8DB(t)
		f := newProjected(t)

		var rows []map[string]any
		require.NoError(t, ApplyGorm(f, db.Model(&gormH8User{}).Distinct("age")).Find(&rows).Error)
		assert.Len(t, rows, 2, "rows=%v", rows)
	})

	t.Run("Distinct chained after ApplyGorm survives", func(t *testing.T) {
		// Order-independence matters: a caller may narrow the handle AFTER
		// ApplyGorm returns, so the fix cannot depend on inspecting
		// Statement.Distinct while applying.
		db := newGormH8DB(t)
		f := newProjected(t)

		var rows []map[string]any
		require.NoError(t, ApplyGorm(f, db.Model(&gormH8User{})).Distinct().Find(&rows).Error)
		assert.Len(t, rows, 2, "rows=%v", rows)
	})

	t.Run("Distinct Count is still distinct", func(t *testing.T) {
		db := newGormH8DB(t)
		f := newProjected(t)

		var n int64
		require.NoError(t, ApplyGorm(f, db.Model(&gormH8User{}).Distinct()).Count(&n).Error)
		assert.Equal(t, int64(2), n, "COUNT over a DISTINCT projection counted duplicates")
	})

	t.Run("Joins relation still scans", func(t *testing.T) {
		db := newGormH8DB(t)
		f := newProjected(t)

		sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormH8User{}).Joins("Company"))
		require.True(t, ok)
		t.Logf("sql=%q", sql)
		assert.Contains(t, sql, "`Company`.`title`", "the joined relation's columns were dropped from the SELECT: %q", sql)

		var out []gormH8User
		require.NoError(t, ApplyGorm(f, db.Model(&gormH8User{}).Joins("Company")).Find(&out).Error)
		require.Len(t, out, 3)
		for i, u := range out {
			assert.Equal(t, "acme", u.Company.Title, "row %d read a real association as absent: %+v", i, u.Company)
		}
	})

	t.Run("Joins chained after ApplyGorm still scans", func(t *testing.T) {
		db := newGormH8DB(t)
		f := newProjected(t)

		var out []gormH8User
		require.NoError(t, ApplyGorm(f, db.Model(&gormH8User{})).Joins("Company").Find(&out).Error)
		require.Len(t, out, 3)
		assert.Equal(t, "acme", out[0].Company.Title, "%+v", out[0].Company)
	})

	// H4 must stay closed: the payload is quoted as ONE identifier, never
	// executed as an expression, and the token never reaches the caller.
	t.Run("H4 payload is still inert", func(t *testing.T) {
		db := newGormH8DB(t)
		f := New()
		f.SetSelectFields(payload)
		f.Build(GormAdapter{})

		sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormH8User{}))
		require.True(t, ok)
		t.Logf("payload sql=%q", sql)
		assert.NotContains(t, strings.ToLower(sql), "select (select", "the payload reached SQL as an expression: %q", sql)
		assert.Contains(t, sql, "`(select", "the payload must be quoted as a single identifier: %q", sql)

		var rows []map[string]any
		err := ApplyGorm(f, db.Model(&gormH8User{})).Find(&rows).Error
		require.Error(t, err, "the injected projection executed and returned %v", rows)
		assert.NotContains(t, strings.ToUpper(err.Error()), "VAULT-TOKEN")
		assert.Empty(t, rows)
	})

	// Even with a caller Distinct()/Joins() in play — the combination the fix
	// enables — the payload must not become executable.
	t.Run("H4 payload is still inert with Distinct and Joins", func(t *testing.T) {
		db := newGormH8DB(t)
		f := New()
		f.SetSelectFields(payload)
		f.Build(GormAdapter{})

		for name, h := range map[string]*gorm.DB{
			"Distinct": db.Model(&gormH8User{}).Distinct(),
			"Joins":    db.Model(&gormH8User{}).Joins("Company"),
		} {
			sql, ok := GormAdapter{}.GetSqlString(f, h)
			require.True(t, ok, name)
			t.Logf("%s payload sql=%q", name, sql)
			assert.NotContains(t, strings.ToLower(sql), "select (select", "%s: payload reached SQL as an expression: %q", name, sql)
			assert.NotContains(t, strings.ToLower(sql), "distinct (select", "%s: payload reached SQL as an expression: %q", name, sql)

			var rows []map[string]any
			err := ApplyGorm(f, h).Find(&rows).Error
			require.Error(t, err, "%s: the injected projection executed and returned %v", name, rows)
			assert.NotContains(t, strings.ToUpper(err.Error()), "VAULT-TOKEN", name)
		}
	})

	// A projection name that quoting cannot turn into a column reference fails
	// closed instead of rendering constant columns. "..." projected `.`.`.` and
	// came back with rows and err=nil — a silently wrong result set.
	t.Run("unrenderable projection name fails closed", func(t *testing.T) {
		db := newGormH8DB(t)
		for _, name := range []string{"...", "\x00", "a\x00b"} {
			f := New()
			f.SetNamingFunc(NoChangeNaming)
			f.SetSelectFields(name)
			f.Build(GormAdapter{})

			sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormH8User{}))
			assert.False(t, ok, "select field %q rendered %q instead of failing closed", name, sql)

			var rows []map[string]any
			assert.Error(t, ApplyGorm(f, db.Model(&gormH8User{})).Find(&rows).Error, "select field %q executed and returned %v", name, rows)
		}
	})

	// Control: an ordinary projection is unchanged — still narrowed, still
	// quoted for schema-resolved columns.
	t.Run("ordinary projection unchanged", func(t *testing.T) {
		db := newGormH8DB(t)
		f := New()
		f.SetSelectFields("age", "id")
		f.Build(GormAdapter{})

		sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormH8User{}))
		require.True(t, ok)
		assert.Equal(t, "SELECT `age`,`id` FROM `gorm_h8_users`", sql)

		var out []gormH8User
		require.NoError(t, ApplyGorm(f, db.Model(&gormH8User{})).Find(&out).Error)
		require.Len(t, out, 3)
		assert.Zero(t, out[0].CompanyID, "company_id was not projected")
	})
}

// G2: a figo.OrderBy in the TOP-LEVEL clause list must render the same ORDER BY
// on the GORM adapter as on the raw adapter.
//
// The divergence A1/A3-3 was filed to close was inverted rather than closed:
// raw started rendering the clause-list form in the same commit in which GORM
// stopped, so one figo instance still returned rows in a different ORDER
// depending on the adapter — and under any take: at all, that is a different
// PAGE of rows. The contract pinned here: honour it at the top level
// on both adapters, clause-list columns first and GetSort's after, and continue
// to ignore it in a boolean/expression position.
func TestG2_ClauseListOrderByRendersOnGormAndRaw(t *testing.T) {
	db := newGormH8DB(t)
	desc := OrderBy{Columns: []OrderByColumn{{Name: "age", Desc: true}}}

	t.Run("both adapters render it", func(t *testing.T) {
		fr := New()
		fr.AddFilter(desc)
		fr.Build(RawAdapter{Dialect: SQLiteDialect})
		rawSQL, ok := RawAdapter{Dialect: SQLiteDialect}.GetSqlString(fr, RawContext{Table: "gorm_h8_users"})
		require.True(t, ok)

		fg := New()
		fg.AddFilter(desc)
		fg.Build(GormAdapter{})
		gormSQL, ok := GormAdapter{}.GetSqlString(fg, db.Model(&gormH8User{}))
		require.True(t, ok)

		t.Logf("raw =%q", rawSQL)
		t.Logf("gorm=%q", gormSQL)
		assert.Contains(t, rawSQL, "ORDER BY")
		assert.Contains(t, gormSQL, "ORDER BY", "the raw adapter orders these rows and the GORM adapter does not: raw=%q gorm=%q", rawSQL, gormSQL)
		assert.Contains(t, gormSQL, "DESC")
	})

	t.Run("executed row order matches", func(t *testing.T) {
		f := New()
		f.AddFilter(desc)
		f.Build(GormAdapter{})

		var out []gormH8User
		require.NoError(t, ApplyGorm(f, db.Model(&gormH8User{})).Find(&out).Error)
		require.Len(t, out, 3)
		ages := []int{out[0].Age, out[1].Age, out[2].Age}
		assert.Equal(t, []int{40, 30, 30}, ages, "rows came back unordered: %v", ages)
	})

	t.Run("clause-list columns precede GetSort", func(t *testing.T) {
		f := New()
		f.AddFilter(desc)
		f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "id", Desc: false}}})
		f.Build(GormAdapter{})

		sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormH8User{}))
		require.True(t, ok)
		t.Logf("sql=%q", sql)
		age, id := strings.Index(sql, "`age`"), strings.Index(sql, "`id`")
		require.NotEqual(t, -1, age, sql)
		require.NotEqual(t, -1, id, sql)
		assert.Less(t, age, id, "clause-list columns must sort before GetSort's: %q", sql)
	})

	// A1/A3-2 must stay closed: in a BOOLEAN position the sort spec still
	// renders as nothing, so it cannot be inlined into WHERE as an
	// unexecutable "AND `t`.`age` DESC".
	t.Run("nested OrderBy is still ignored", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			expr Expr
		}{
			{"And", AndExpr{Operands: []Expr{EqExpr{Field: "age", Value: 30}, desc}}},
			{"Or", OrExpr{Operands: []Expr{EqExpr{Field: "age", Value: 30}, desc}}},
			{"Not", NotExpr{Operands: []Expr{desc}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := New()
				f.AddFilter(tc.expr)
				f.Build(GormAdapter{})

				sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormH8User{}))
				require.True(t, ok)
				t.Logf("sql=%q", sql)
				assert.NotContains(t, sql, "DESC", "a nested sort spec was inlined into the predicate: %q", sql)

				var out []gormH8User
				assert.NoError(t, ApplyGorm(f, db.Model(&gormH8User{})).Find(&out).Error, "sql=%q", sql)
			})
		}
	})
}

// G3 (third strand): an already-qualified sort key must not be qualified again.
//
// gormOrderByClause set clause.Column{Table: clause.CurrentTable}
// unconditionally, so a dotted name got a THIRD qualifier prepended and the
// adapter returned unexecutable SQL with ok=true ("no such column:
// t.t.age"). Pre-existing, identical at the parent commit.
func TestG3_DottedSortKeyIsNotRequalified(t *testing.T) {
	db := newGormH8DB(t)

	// NoChangeNaming isolates this strand from the identifier-normalization
	// defect (the default naming func folds "t.age" to "t_age" before the
	// adapter ever sees it — a separate finding, in figo.go/raw.go).
	f := New()
	f.SetNamingFunc(NoChangeNaming)
	f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "gorm_h8_users.age", Desc: true}}})
	f.Build(GormAdapter{})

	sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormH8User{}))
	require.True(t, ok)
	t.Logf("sql=%q", sql)
	assert.Contains(t, sql, "ORDER BY `gorm_h8_users`.`age` DESC", "sql=%q", sql)
	assert.NotContains(t, sql, "`gorm_h8_users`.`gorm_h8_users`", "the sort key was qualified twice: %q", sql)

	var out []gormH8User
	require.NoError(t, ApplyGorm(f, db.Model(&gormH8User{})).Find(&out).Error, "sql=%q", sql)
	require.Len(t, out, 3)
	assert.Equal(t, 40, out[0].Age, "rows came back unordered")

	// Control: an UNqualified sort key still gets the current table, which is
	// what disambiguates it under a JOIN.
	f2 := New()
	f2.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "age", Desc: true}}})
	f2.Build(GormAdapter{})
	sql2, ok := GormAdapter{}.GetSqlString(f2, db.Model(&gormH8User{}))
	require.True(t, ok)
	assert.Contains(t, sql2, "ORDER BY `gorm_h8_users`.`age` DESC", "sql=%q", sql2)
}
