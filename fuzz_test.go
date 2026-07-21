package figo_test

import (
	"testing"

	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"
)

// FuzzParseDSL drives arbitrary input through the full pipeline — parse,
// build, render, explain, clone, walk. A hand-rolled string parser is the
// canonical fuzz target: the property under test is "no input panics", not
// output correctness (BuildE's diagnostics cover semantic drops).
//
// `go test` runs the seed corpus below on every CI run; use
// `go test -fuzz=FuzzParseDSL` locally to explore beyond it.
func FuzzParseDSL(f *testing.F) {
	seeds := []string{
		// Representative valid DSL.
		`name="john" and age>25`,
		`status = "active" and (a=1 or b=2)`,
		`age <bet> (10..20) and id<in>[1,2,3]`,
		`x<nin>["a,b","c"] or y<null> or z<notnull>`,
		`name=^"%jo%" and mail.=^"%X%" and r=~"^ab" and nr!=~"z$"`,
		`sort=name:asc,created_at:desc page=skip:10,take:5`,
		`id>0 load=[Orders:total>100 | Profile:bio=^"%dev%"]`,
		`not deleted=true and active=true`,
		`created=2024-01-02 and code="0123" and big=99999999999999999999999`,
		`سن > 5 and émail = "x"`,
		// Historical bug triggers, kept as regression seeds.
		`name=x load=[Phone:sort=id:desc and id=1]`,        // preload sort= leak
		`name=x load=[Phone:page=skip:90,take:5 and id=1]`, // preload page= leak
		`name=x load=[A:load=[B:z=1] and y=2]`,             // nested load= flattening
		`a=1 b=2 or c=3`,                                   // implicit-AND precedence
		`a=1 not b=2`,                                      // positional NOT binding
		`name="a)b" and note="a[b"`,                        // quoted structural chars
		`name = = 5`,                                       // doubled operator
		`load=[`,                                           // unclosed directive
		`page=skip:`,                                       // malformed directive
		`sort=`,                                            // empty directive
		`=5`,                                               // operator with no field
		`(((((`,                                            // unbalanced groups
		`"unclosed`,                                        // unclosed quote
		`a<bet>(1..`,                                       // unclosed range
		`x<in>[1,2`,                                        // unclosed list
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, dsl string) {
		fg := New()
		if err := fg.AddFiltersFromString(dsl); err != nil {
			return
		}
		_ = fg.BuildE(RawAdapter{})
		_ = fg.Explain()
		_ = fg.GetSqlString("t")
		_ = fg.GetQuery("t")

		c := fg.Clone()
		c.Walk(func(Expr) {})
		_ = c.BuildE(nil)
	})
}
