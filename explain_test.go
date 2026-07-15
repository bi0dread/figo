package figo_test

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func explainFor(t *testing.T, dsl string) string {
	t.Helper()
	f := New()
	if err := f.AddFiltersFromString(dsl); err != nil {
		t.Fatalf("AddFiltersFromString(%q): %v", dsl, err)
	}
	f.Build(RawAdapter{})
	return f.Explain()
}

// TestExplainMatchesExample checks the exact tree rendering from the feature request.
func TestExplainMatchesExample(t *testing.T) {
	got := explainFor(t, `id=1 and (age>20 or active=true)`)
	want := "AND\n" +
		" ├── id = 1\n" +
		" └── OR\n" +
		"     ├── age > 20\n" +
		"     └── active = true\n"
	fmt.Println(got)
	assert.Equal(t, want, got)
}

func TestExplainVariants(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		f := New()
		f.Build(RawAdapter{})
		assert.Equal(t, "(no filters)", f.Explain())
	})

	t.Run("NotIsShown", func(t *testing.T) {
		got := explainFor(t, `not status="deleted"`)
		want := "NOT\n" +
			" └── status = \"deleted\"\n"
		assert.Equal(t, want, got)
	})

	t.Run("StringsQuotedNumbersAndBoolsBare", func(t *testing.T) {
		got := explainFor(t, `name="john" and age>20 and active=true`)
		assert.Contains(t, got, `name = "john"`)
		assert.Contains(t, got, "age > 20")
		assert.Contains(t, got, "active = true")
	})

	t.Run("SetAndRangeOps", func(t *testing.T) {
		got := explainFor(t, `id<in>[1,2,3] and price<bet>(10..20)`)
		assert.Contains(t, got, "id IN [1, 2, 3]")
		assert.Contains(t, got, "price BETWEEN 10 AND 20")
	})
}
