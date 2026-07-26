package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type gormH7Account struct {
	ID       int
	TenantID string
	Secret   string
}

type gormH7Vault struct {
	ID    int
	Token string
}

func newGormH7DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&gormH7Account{}, &gormH7Vault{}))
	require.NoError(t, db.Create(&gormH7Account{ID: 1, TenantID: "A", Secret: "alpha-secret"}).Error)
	require.NoError(t, db.Create(&gormH7Account{ID: 2, TenantID: "B", Secret: "beta-secret"}).Error)
	require.NoError(t, db.Create(&gormH7Vault{ID: 1, Token: "VAULT-TOKEN-XYZ"}).Error)
	return db
}

// H2: a *gorm.DB reused across figo instances (the common `base :=
// db.Model(&X{})` pattern) must render each instance's own filters. ApplyGorm
// used to mark and mutate the caller's handle in place, so the second instance
// was skipped entirely and served the FIRST instance's filter — a cross-tenant
// leak with ok=true and no error.
func TestH2_ReusedGormDBDoesNotServePreviousInstanceFilters(t *testing.T) {
	db := newGormH7DB(t)
	base := db.Model(&gormH7Account{})

	for _, tenant := range []string{"A", "B"} {
		f := New()
		f.AddFilter(EqExpr{Field: "tenant_id", Value: tenant})
		f.Build(GormAdapter{})

		q, ok := GormAdapter{}.GetQuery(f, base)
		require.True(t, ok)
		sq, isSQL := q.(SQLQuery)
		require.True(t, isSQL)
		t.Logf("tenant %s sql=%q args=%v", tenant, sq.SQL, sq.Args)
		assert.Equal(t, []any{tenant}, sq.Args, "tenant %s got another instance's bind vars", tenant)

		var got []gormH7Account
		require.NoError(t, ApplyGorm(f, base).Find(&got).Error)
		require.Len(t, got, 1, "tenant %s: expected exactly its own row", tenant)
		assert.Equal(t, tenant, got[0].TenantID, "tenant %s received another tenant's row (%s/%s)", tenant, got[0].TenantID, got[0].Secret)
	}
}

// H2 (direct form): calling ApplyGorm twice on the same caller handle must not
// accumulate both instances' predicates onto it.
func TestH2_ApplyGormDoesNotMutateCallerHandle(t *testing.T) {
	db := newGormH7DB(t)
	base := db.Model(&gormH7Account{})

	f1 := New()
	f1.AddFilter(EqExpr{Field: "tenant_id", Value: "A"})
	f1.Build(GormAdapter{})
	_ = ApplyGorm(f1, base)

	f2 := New()
	f2.AddFilter(EqExpr{Field: "tenant_id", Value: "B"})
	f2.Build(GormAdapter{})
	q, ok := GormAdapter{}.GetQuery(f2, base)
	require.True(t, ok)
	sq := q.(SQLQuery)
	t.Logf("second render sql=%q args=%v", sq.SQL, sq.Args)
	assert.Equal(t, []any{"B"}, sq.Args, "the first instance's state leaked onto the caller's handle")

	// The caller's own handle must still be a clean, unfiltered scope.
	var got []gormH7Account
	require.NoError(t, base.Find(&got).Error)
	assert.Len(t, got, 2, "figo filtered the caller's own handle")
}

// H3: rendering a SEGMENT ("WHERE", "SORT", ...) must not leave the fragment in
// the caller's statement buffer. It used to, and the caller's next Find failed
// with `near "WHERE": syntax error`.
func TestH3_SegmentRenderDoesNotCorruptCallerDB(t *testing.T) {
	db := newGormH7DB(t)
	tx := db.Model(&gormH7Account{})

	f := New()
	f.AddFilter(EqExpr{Field: "tenant_id", Value: "A"})
	f.Build(GormAdapter{})

	seg, ok := GormAdapter{}.GetSqlString(f, tx, "WHERE")
	require.True(t, ok)
	assert.Contains(t, seg, "tenant_id", "the segment itself must still render: %q", seg)

	t.Logf("caller statement after segment render: sql=%q vars=%v", tx.Statement.SQL.String(), tx.Statement.Vars)
	assert.Empty(t, tx.Statement.SQL.String(), "segment render left a fragment in the caller's statement")
	assert.Empty(t, tx.Statement.Vars, "segment render left bind vars in the caller's statement")

	var got []gormH7Account
	require.NoError(t, tx.Find(&got).Error, "the caller's next query must still work")
	assert.Len(t, got, 2)
}

// M4: README documents that read-render methods are safe to call concurrently
// after Build. Rendering through one shared derived *gorm.DB used to race
// (ApplyGorm writing the shared clause map against Find cloning it). Run this
// with -race.
func TestM4_ConcurrentRenderOnSharedGormDBIsRaceFree(t *testing.T) {
	db := newGormH7DB(t)
	shared := db.Model(&gormH7Account{})

	f := New()
	f.AddFilter(EqExpr{Field: "tenant_id", Value: "A"})
	f.Build(GormAdapter{})

	const n = 8
	sqls := make([]string, n)
	oks := make([]bool, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sqls[i], oks[i] = GormAdapter{}.GetSqlString(f, shared)
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		require.True(t, oks[i], "render %d failed", i)
		assert.Equal(t, sqls[0], sqls[i], "concurrent renders diverged: %q vs %q", sqls[0], sqls[i])
		assert.Contains(t, sqls[i], "tenant_id")
	}
}

// H4: the SELECT projection was emitted UNQUOTED (GORM renders an unresolvable
// select name as clause.Column{Raw:true}), so a crafted field name executed as
// SQL and exfiltrated another table. Projection identifiers must be quoted like
// the raw adapter's.
func TestH4_SelectProjectionIsQuotedNotInjected(t *testing.T) {
	db := newGormH7DB(t)
	payload := "(SELECT/**/token/**/FROM/**/gorm_h7_vaults)AS/**/secret"

	f := New()
	f.SetSelectFields(payload)
	f.Build(GormAdapter{})

	sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormH7Account{}))
	require.True(t, ok)
	t.Logf("projection sql=%q", sql)
	assert.NotContains(t, strings.ToLower(sql), "select (select", "the payload reached SQL as an expression: %q", sql)
	assert.Contains(t, sql, "`(select", "the payload must be quoted as a single identifier: %q", sql)

	var rows []map[string]any
	err := ApplyGorm(f, db.Model(&gormH7Account{})).Find(&rows).Error
	t.Logf("projection exec err=%v rows=%v", err, rows)
	require.Error(t, err, "the injected projection executed and returned %v", rows)
	assert.NotContains(t, strings.ToUpper(err.Error()), "VAULT-TOKEN")
}

// H4 control: an ordinary projection must keep working and stay quoted.
func TestH4_OrdinarySelectProjectionStillWorks(t *testing.T) {
	db := newGormH7DB(t)

	f := New()
	f.SetSelectFields("secret", "tenant_id")
	f.Build(GormAdapter{})

	sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormH7Account{}))
	require.True(t, ok)
	t.Logf("ordinary projection sql=%q", sql)
	assert.Contains(t, sql, "`secret`")
	assert.Contains(t, sql, "`tenant_id`")
	assert.NotContains(t, sql, "`id`", "only the selected columns must be projected: %q", sql)

	var got []gormH7Account
	require.NoError(t, ApplyGorm(f, db.Model(&gormH7Account{})).Find(&got).Error)
	require.Len(t, got, 2)
	assert.Equal(t, "alpha-secret", got[0].Secret)
	assert.Zero(t, got[0].ID, "id was not projected")

	// The projection now lives in the SELECT clause instead of
	// Statement.Selects, so check the other finisher that rewrites SELECT.
	var n int64
	require.NoError(t, ApplyGorm(f, db.Model(&gormH7Account{})).Count(&n).Error)
	assert.Equal(t, int64(2), n)
}

// A3-2: a figo.OrderBy nested in a boolean position (And/Or/Not operand) used
// to render as an ORDER BY fragment inside the WHERE clause — unexecutable SQL
// returned with ok=true. The GORM adapter now ignores it there, exactly like
// the raw adapter does.
func TestA3_2_OrderByInBooleanPositionIsIgnored(t *testing.T) {
	db := newGormH7DB(t)
	ob := OrderBy{Columns: []OrderByColumn{{Name: "id", Desc: true}}}

	cases := []struct {
		name     string
		expr     Expr
		wantRows int
	}{
		{"And", AndExpr{Operands: []Expr{EqExpr{Field: "tenant_id", Value: "A"}, ob}}, 1},
		{"Or", OrExpr{Operands: []Expr{EqExpr{Field: "tenant_id", Value: "A"}, ob}}, 1},
		{"Not", NotExpr{Operands: []Expr{ob}}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := New()
			f.AddFilter(tc.expr)
			f.Build(GormAdapter{})

			q, ok := GormAdapter{}.GetQuery(f, db.Model(&gormH7Account{}))
			require.True(t, ok)
			sq := q.(SQLQuery)
			t.Logf("%s sql=%q args=%v", tc.name, sq.SQL, sq.Args)
			assert.NotContains(t, sq.SQL, "DESC", "an ORDER BY fragment leaked into the WHERE clause: %q", sq.SQL)

			var got []gormH7Account
			err := ApplyGorm(f, db.Model(&gormH7Account{})).Find(&got).Error
			require.NoError(t, err, "rendered SQL is not executable: %q", sq.SQL)
			assert.Len(t, got, tc.wantRows)
		})
	}
}

// A3-2 control: the sort= directive must still render an ORDER BY.
func TestA3_2_SortDirectiveStillRendersOrderBy(t *testing.T) {
	db := newGormH7DB(t)

	f := New()
	require.NoError(t, f.AddFiltersFromString(`tenant_id="A" sort=id:desc`))
	f.Build(GormAdapter{})

	sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormH7Account{}))
	require.True(t, ok)
	t.Logf("sort sql=%q", sql)
	assert.Contains(t, sql, "ORDER BY")
	assert.Contains(t, sql, "DESC")
}

// A1/A3-4 (the GORM half) — GORM's clause.Expr splits ANY slice bound to a '?'
// that follows '(' into one placeholder per element and exempts only
// driver.Valuer, so a CustomExpr handler returning "hash IN (?)" with a []byte
// blob rendered "hash IN (?,?)" bound to the individual BYTES: zero rows,
// err=nil, while the raw adapter (same documented handler contract) bound the
// blob and matched the row.
type byteBlobRow struct {
	ID   uint `gorm:"primarykey"`
	Hash []byte
}

func TestA3_4_GormDoesNotShredAByteSliceBindArg(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&byteBlobRow{}))
	require.NoError(t, db.Create(&byteBlobRow{ID: 1, Hash: []byte{0xde, 0xad}}).Error)

	mk := func() Figo {
		f := New()
		f.AddFilter(CustomExpr{Field: "hash", Operator: "eq",
			Handler: func(field, op string, v any) (string, []any, error) {
				return "hash IN (?)", []any{[]byte{0xde, 0xad}}, nil
			}})
		return f
	}

	g := mk()
	g.Build(GormAdapter{})
	q, ok := GormAdapter{}.GetQuery(g, db.Model(&byteBlobRow{}))
	require.True(t, ok)
	sq := q.(SQLQuery)
	assert.Contains(t, sq.SQL, "IN (?)", "the blob must stay ONE placeholder: %s", sq.SQL)
	assert.NotContains(t, sq.SQL, "IN (?,?)")

	var got []byteBlobRow
	require.NoError(t, ApplyGorm(mk(), db.Model(&byteBlobRow{})).Find(&got).Error)
	assert.Len(t, got, 1, "the blob must match its row, as it does on the raw adapter")

	// Parity check: a non-byte slice is still expanded, which is GORM's rule
	// for "IN (?)" and what the raw adapter mirrors in expandSliceArgs.
	r := New()
	r.AddFilter(CustomExpr{Field: "id", Operator: "in",
		Handler: func(field, op string, v any) (string, []any, error) {
			return "id IN (?)", []any{[]any{int64(1), int64(2)}}, nil
		}})
	r.Build(GormAdapter{})
	q2, ok2 := GormAdapter{}.GetQuery(r, db.Model(&byteBlobRow{}))
	require.True(t, ok2)
	assert.Contains(t, q2.(SQLQuery).SQL, "IN (?,?)", "a real value list must still expand")
}
