package adapters

import (
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func round4GormDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return db.Session(&gorm.Session{NewDB: true}).Table("users")
}

// A single-operand OrExpr is just that operand. GORM's clause.Where treats a
// clause.OrConditions holding exactly one expression as an "or condition" and
// joins it to its sibling with OR (and swaps it off index 0 to do so), so
// rendering it via clause.Or turned every other top-level clause into a
// disjunct — silently widening the result set.
func TestGormSingleOperandOrIsAndedWithSiblings(t *testing.T) {
	f := figo.New()
	f.AddFilter(figo.EqExpr{Field: "b", Value: 2})
	f.AddFilter(figo.OrExpr{Operands: []figo.Expr{figo.EqExpr{Field: "a", Value: 1}}})
	f.Build(GormAdapter{})

	sql := f.GetSqlString(round4GormDB(t), "WHERE")
	if strings.Contains(sql, " OR ") {
		t.Fatalf("single-operand OrExpr must not OR with its sibling clause, got %q", sql)
	}
	if !strings.Contains(sql, " AND ") {
		t.Fatalf("want the two top-level clauses ANDed, got %q", sql)
	}
}

// The same shape nested inside an AndExpr: an explicit AND must never render OR.
func TestGormSingleOperandOrInsideAndStaysAnd(t *testing.T) {
	f := figo.New()
	f.AddFilter(figo.AndExpr{Operands: []figo.Expr{
		figo.EqExpr{Field: "a", Value: 1},
		figo.OrExpr{Operands: []figo.Expr{figo.EqExpr{Field: "b", Value: 2}}},
	}})
	f.Build(GormAdapter{})

	sql := f.GetSqlString(round4GormDB(t), "WHERE")
	if strings.Contains(sql, " OR ") {
		t.Fatalf("AndExpr operands must render as AND, got %q", sql)
	}
}

// A multi-operand OrExpr keeps its disjunction, parenthesized against siblings.
func TestGormMultiOperandOrStillOrs(t *testing.T) {
	f := figo.New()
	f.AddFilter(figo.OrExpr{Operands: []figo.Expr{
		figo.EqExpr{Field: "a", Value: 1},
		figo.EqExpr{Field: "b", Value: 2},
	}})
	f.AddFilter(figo.EqExpr{Field: "tenant_id", Value: "T1"})
	f.Build(GormAdapter{})

	sql := f.GetSqlString(round4GormDB(t), "WHERE")
	if !strings.Contains(sql, " OR ") || !strings.Contains(sql, " AND ") {
		t.Fatalf("want (a OR b) AND tenant_id, got %q", sql)
	}
	if strings.Index(sql, " OR ") > strings.Index(sql, " AND ") {
		t.Fatalf("the OR group must precede the ANDed sibling, got %q", sql)
	}
}

// All four adapters must agree that a single-operand OrExpr is a conjunct.
func TestSingleOperandOrParityAcrossAdapters(t *testing.T) {
	mk := func() figo.Figo {
		f := figo.New()
		f.AddFilter(figo.OrExpr{Operands: []figo.Expr{figo.EqExpr{Field: "a", Value: 1}}})
		f.AddFilter(figo.EqExpr{Field: "tenant_id", Value: "T1"})
		return f
	}

	fr := mk()
	fr.Build(RawAdapter{})
	where, _, err := BuildRawWhere(fr)
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	if !strings.Contains(where, " AND ") || strings.Contains(where, " OR ") {
		t.Fatalf("raw must AND the two clauses, got %q", where)
	}

	fg := mk()
	fg.Build(GormAdapter{})
	if sql := fg.GetSqlString(round4GormDB(t), "WHERE"); strings.Contains(sql, " OR ") {
		t.Fatalf("gorm must AND the two clauses, got %q", sql)
	}
}
