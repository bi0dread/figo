package figo

import "testing"

// A sort set programmatically belongs to the caller and must survive Build,
// exactly as SetPage's value does. Build cleared the sort unconditionally
// before re-parsing the DSL, so SetSort before Build was silently a no-op on
// any instance that also carried a DSL.
func TestSetSortSurvivesBuildWithDSL(t *testing.T) {
	f := New()
	if err := f.AddFiltersFromString(`a=1`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "name", Desc: true}}})
	f.Build(nil)

	got := f.GetSort()
	if got == nil {
		t.Fatal("SetSort before Build was discarded")
	}
	if len(got.Columns) != 1 || got.Columns[0].Name != "name" || !got.Columns[0].Desc {
		t.Fatalf("want name DESC, got %+v", got.Columns)
	}
}

// A sort= directive still wins over a programmatic one, matching page=.
func TestDSLSortOverridesSetSort(t *testing.T) {
	f := New()
	if err := f.AddFiltersFromString(`a=1 sort=id:asc`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "name", Desc: true}}})
	f.Build(nil)

	got := f.GetSort()
	if got == nil || len(got.Columns) != 1 || got.Columns[0].Name != "id" {
		t.Fatalf("want the DSL sort= to win, got %+v", got)
	}
}

// A DSL-derived sort is still cleared when the DSL is replaced by one without
// a sort= — it must not leak into the next query.
func TestDSLSortClearedOnDSLReplacement(t *testing.T) {
	f := New()
	if err := f.AddFiltersFromString(`a=1 sort=id:asc`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)
	if f.GetSort() == nil {
		t.Fatal("want the DSL sort to apply on the first build")
	}

	if err := f.AddFiltersFromString(`b=2`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)
	if got := f.GetSort(); got != nil {
		t.Fatalf("DSL sort leaked into the replacement DSL: %+v", got)
	}
}

// Clearing the DSL entirely also drops a DSL-derived sort but keeps a
// caller-set one.
func TestSortOriginAcrossDSLClear(t *testing.T) {
	fromDSL := New()
	if err := fromDSL.AddFiltersFromString(`a=1 sort=id:asc`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	fromDSL.Build(nil)
	if err := fromDSL.AddFiltersFromString(""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	fromDSL.Build(nil)
	if got := fromDSL.GetSort(); got != nil {
		t.Fatalf("DSL sort survived the clear: %+v", got)
	}

	fromCaller := New()
	if err := fromCaller.AddFiltersFromString(`a=1`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	fromCaller.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "name"}}})
	fromCaller.Build(nil)
	if err := fromCaller.AddFiltersFromString(""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	fromCaller.Build(nil)
	if got := fromCaller.GetSort(); got == nil || got.Columns[0].Name != "name" {
		t.Fatalf("caller-set sort should survive the clear, got %+v", got)
	}
}

// A sort= whose every segment is malformed adopts nothing, so it must not
// claim a caller-set sort as DSL-derived.
func TestMalformedSortLeavesCallerSortAlone(t *testing.T) {
	f := New()
	if err := f.AddFiltersFromString(`a=1 sort=:asc`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "name"}}})
	f.Build(nil)

	if got := f.GetSort(); got == nil || got.Columns[0].Name != "name" {
		t.Fatalf("want the caller sort preserved, got %+v", got)
	}
}

// SetSelectFields replaces the projection set; no arguments clears it.
func TestSetSelectFieldsReplaces(t *testing.T) {
	f := New()
	f.AddSelectFields("a", "b")
	f.SetSelectFields("c")

	got := f.GetSelectFields()
	if len(got) != 1 || !got["c"] {
		t.Fatalf("want exactly {c}, got %v", got)
	}

	f.SetSelectFields()
	if got := f.GetSelectFields(); len(got) != 0 {
		t.Fatalf("want an empty set, got %v", got)
	}
}

// Clone carries the sort origin, so a clone rebuilds with the same semantics.
func TestCloneCarriesSortOrigin(t *testing.T) {
	f := New()
	if err := f.AddFiltersFromString(`a=1`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "name"}}})
	f.Build(nil)

	c := f.Clone()
	c.Build(nil)
	if got := c.GetSort(); got == nil || got.Columns[0].Name != "name" {
		t.Fatalf("clone lost the caller-set sort on rebuild: %+v", got)
	}
}
