package plugins

import (
	"testing"

	figo "github.com/bi0dread/figo/v4"
)

// An ignored field must not survive in the projection either: pruning it from
// the WHERE clause while still SELECTing it returned the very column the
// policy forbids.
func TestFieldsPluginPrunesIgnoredSelectField(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("salary")

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	f.AddSelectFields("salary", "name")
	if err := f.AddFiltersFromString(`name="x"`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)

	got := f.GetSelectFields()
	if got["salary"] {
		t.Fatalf("ignored field still projected: %v", got)
	}
	if !got["name"] {
		t.Fatalf("permitted field was dropped: %v", got)
	}
}

// The whitelist applies to the projection too.
func TestFieldsPluginWhitelistPrunesSelectFields(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.SetAllowedFields("name", "id")
	fp.EnableFieldWhitelist()

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	f.AddSelectFields("name", "salary")
	if err := f.AddFiltersFromString(`name="x"`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)

	got := f.GetSelectFields()
	if got["salary"] || !got["name"] {
		t.Fatalf("want only the whitelisted column projected, got %v", got)
	}
}

// Registered names match in either spelling, as they do for filters and sort.
func TestFieldsPluginSelectFieldNamingSpellings(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("userSalary") // registered pre-naming

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	f.AddSelectFields("user_salary", "name") // requested post-naming
	if err := f.AddFiltersFromString(`name="x"`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)

	if got := f.GetSelectFields(); got["user_salary"] {
		t.Fatalf("converted spelling of an ignored field still projected: %v", got)
	}
}

// Pruning must never WIDEN the projection. An empty select set means "every
// column", so when nothing survives, fall back to the whitelist rather than
// letting the projection become SELECT *.
func TestFieldsPluginNeverWidensProjectionToStar(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.SetAllowedFields("name")
	fp.EnableFieldWhitelist()

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	f.AddSelectFields("salary") // the only requested column is forbidden
	if err := f.AddFiltersFromString(`name="x"`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)

	got := f.GetSelectFields()
	if len(got) == 0 {
		t.Fatal("projection collapsed to SELECT *, exposing every column")
	}
	if got["salary"] || !got["name"] {
		t.Fatalf("want the allowed list substituted, got %v", got)
	}
}

// With only an ignore list there is no safe narrower projection to substitute,
// so the caller's set is left alone rather than widened to SELECT *.
func TestFieldsPluginIgnoreOnlyDoesNotWidenProjection(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("salary")

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	f.AddSelectFields("salary")
	if err := f.AddFiltersFromString(`name="x"`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)

	if got := f.GetSelectFields(); len(got) == 0 {
		t.Fatal("projection collapsed to SELECT *, which is wider than requested")
	}
}

// An instance with no select fields keeps rendering SELECT *: projection
// defaults belong to the application.
func TestFieldsPluginLeavesEmptyProjectionAlone(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("salary")

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := f.AddFiltersFromString(`name="x"`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)

	if got := f.GetSelectFields(); len(got) != 0 {
		t.Fatalf("want the projection left empty, got %v", got)
	}
}

// Pruning a DSL-derived sort must not make it look caller-owned: the next
// rebuild from a sort-less DSL still has to clear it.
func TestFieldsPluginSortPruningKeepsDSLOrigin(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("salary")

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := f.AddFiltersFromString(`name="x" sort=salary:desc,name:asc`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)

	got := f.GetSort()
	if got == nil || len(got.Columns) != 1 || got.Columns[0].Name != "name" {
		t.Fatalf("want only the permitted sort column, got %+v", got)
	}

	if err := f.AddFiltersFromString(`name="y"`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)
	if got := f.GetSort(); got != nil {
		t.Fatalf("pruned DSL sort leaked into the replacement DSL: %+v", got)
	}
}
