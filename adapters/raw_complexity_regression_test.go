package adapters

import (
	"strings"
	"testing"
	"time"

	figo "github.com/bi0dread/figo/v4"
)

// Rendering a long connector chain must be linear. With the left-nested AST
// the SQL builder re-joined and re-parenthesized the whole accumulated string
// at every level: 8 000 conjuncts took 387ms and 50 000 took twelve seconds.
func TestRawRenderOfLongChainIsLinear(t *testing.T) {
	const n = 20000
	f := figo.New()
	if err := f.AddFiltersFromString(strings.Repeat("a=1 and ", n) + "b=2"); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(RawAdapter{})

	start := time.Now()
	where, args, err := BuildRawWhere(f)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("BuildRawWhere: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("rendering a %d-term chain took %v; the quadratic render is back", n, elapsed)
	}
	if len(args) != n+1 {
		t.Fatalf("want %d args, got %d", n+1, len(args))
	}
	// One flat group, not one paren pair per conjunct.
	if got := strings.Count(where, "("); got != 1 {
		t.Fatalf("want a single paren group, got %d", got)
	}
}

// A flat conjunction renders as one group rather than N nested ones.
func TestRawRenderFlatConjunction(t *testing.T) {
	f := figo.New()
	if err := f.AddFiltersFromString(`a=1 and b=2 and c=3 and d=4`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(RawAdapter{})

	where, _, err := BuildRawWhere(f)
	if err != nil {
		t.Fatalf("BuildRawWhere: %v", err)
	}
	want := "(`a` = ? AND `b` = ? AND `c` = ? AND `d` = ?)"
	if where != want {
		t.Fatalf("want %q, got %q", want, where)
	}
}

// Precedence still renders correctly after flattening.
func TestRawRenderMixedPrecedence(t *testing.T) {
	f := figo.New()
	if err := f.AddFiltersFromString(`a=1 and b=2 or c=3 and d=4`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(RawAdapter{})

	where, _, err := BuildRawWhere(f)
	if err != nil {
		t.Fatalf("BuildRawWhere: %v", err)
	}
	want := "((`a` = ? AND `b` = ?) OR (`c` = ? AND `d` = ?))"
	if where != want {
		t.Fatalf("want %q, got %q", want, where)
	}
}

// The adapter's full-SELECT path must render each segment once. Requesting the
// whole statement used to build every segment, throw them away, and rebuild
// them inside buildFullSelect.
func TestRawFullSelectDoesNotDoubleRender(t *testing.T) {
	const n = 4000
	f := figo.New()
	if err := f.AddFiltersFromString(strings.Repeat("a=1 and ", n) + "b=2"); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(RawAdapter{})

	start := time.Now()
	direct, _, err := BuildRawSelect(f, "users")
	if err != nil {
		t.Fatalf("BuildRawSelect: %v", err)
	}
	directTime := time.Since(start)

	start = time.Now()
	q, ok := RawAdapter{}.GetQuery(f, "users")
	if !ok {
		t.Fatal("RawAdapter.GetQuery failed")
	}
	adapterTime := time.Since(start)

	if sq, ok := q.(figo.SQLQuery); !ok || sq.SQL != direct {
		t.Fatalf("adapter SQL diverged from BuildRawSelect")
	}
	// Allow generous slack for scheduling noise; the bug made this ~2x.
	if adapterTime > 8*directTime+10*time.Millisecond {
		t.Fatalf("adapter path %v vs direct %v: segments are being rendered twice", adapterTime, directTime)
	}
}
