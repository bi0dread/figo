package plugins

// Round-3 regression tests: FieldsPlugin sort enforcement and the
// finalizer-free inspection clone used by Limits/Validation.

import (
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
)

// Was: sort= bypassed both the whitelist and the ignore list — the WHERE
// condition on a forbidden column was pruned but ORDER BY <forbidden> survived,
// an oracle for leaking the column's values via ordering + take:1.
func TestSortRespectsWhitelistAndIgnore(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.SetAllowedFields("id", "a")
	fp.EnableFieldWhitelist()

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatal(err)
	}
	_ = f.AddFiltersFromString("a=1 salary=99999 sort=salary:desc page=take:1")
	f.Build(adapters.RawAdapter{})

	sql, _, err := adapters.BuildRawSelect(f, "users")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "salary") {
		t.Errorf("forbidden column leaked into SQL: %q", sql)
	}
	if strings.Contains(sql, "ORDER BY") {
		t.Errorf("ORDER BY should be gone entirely: %q", sql)
	}

	// Allowed sort columns must survive, forbidden ones are pruned from a
	// mixed directive.
	f2 := figo.New()
	fp2 := NewFieldsPlugin()
	fp2.AddIgnoreFields("secret")
	if err := f2.RegisterPlugin(fp2); err != nil {
		t.Fatal(err)
	}
	_ = f2.AddFiltersFromString("a=1 sort=secret:desc,id:asc")
	f2.Build(adapters.RawAdapter{})
	sql2, _, err := adapters.BuildRawSelect(f2, "users")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql2, "secret") {
		t.Errorf("ignored column leaked into SQL: %q", sql2)
	}
	if !strings.Contains(sql2, "ORDER BY `id`") {
		t.Errorf("allowed sort column was lost: %q", sql2)
	}
}

// Was: LimitsPlugin measured the clone AFTER clause finalizers ran on it, so a
// ScopePlugin's injected tenant filter counted against the user's limits and
// legitimate queries at the boundary were rejected.
func TestLimitsIgnoreScopeInjectedClauses(t *testing.T) {
	scope := NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: 42})
	limits := NewLimitsPlugin(QueryLimits{MaxFieldCount: 1})

	f := figo.New()
	if err := f.RegisterPlugin(scope); err != nil {
		t.Fatal(err)
	}
	if err := f.RegisterPlugin(limits); err != nil {
		t.Fatal(err)
	}

	// One user field, limit 1: must pass even though the scope adds a second
	// field to the final query.
	if err := f.AddFiltersFromString("a=1"); err != nil {
		t.Errorf("AddFiltersFromString: %v (scope clause counted against user limits)", err)
	}

	// The scope must still be enforced on the real build.
	f.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(where, "tenant_id") {
		t.Errorf("scope clause missing from the real query: %q", where)
	}

	// Two user fields still trip the limit.
	if err := f.AddFiltersFromString("a=1 and b=2"); err == nil {
		t.Error("expected MaxFieldCount rejection for two user fields")
	}
}

// Was: ValidationPlugin validated the clone AFTER finalizers, so rules fired
// against a ScopePlugin's injected values and rejected legitimate queries.
func TestValidationIgnoresScopeInjectedClauses(t *testing.T) {
	scope := NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: -1})
	vp := NewValidationPlugin()
	vp.AddRule(ValidationRule{
		Field: "tenant_id",
		Rule:  "min",
		Value: 0,
	})

	f := figo.New()
	if err := f.RegisterPlugin(scope); err != nil {
		t.Fatal(err)
	}
	if err := f.RegisterPlugin(vp); err != nil {
		t.Fatal(err)
	}

	// The user's DSL never touches tenant_id; the scope's value (-1, below the
	// rule's min) must not be validated.
	if err := f.AddFiltersFromString("a=1"); err != nil {
		t.Errorf("AddFiltersFromString: %v (scope value was validated)", err)
	}

	// A user-supplied violating value must still be rejected.
	if err := f.AddFiltersFromString("tenant_id=-5"); err == nil {
		t.Error("expected a validation error for the user-supplied value")
	}
}

// Was: with AuditPlugin registered AFTER a rejecting plugin, ExecuteAfterParse
// short-circuited and the rejected parse attempt was never recorded at all.
func TestAuditRecordsRejectedParseRegardlessOfOrder(t *testing.T) {
	for _, auditFirst := range []bool{true, false} {
		audit := NewAuditPlugin(nil, 10)
		limits := NewLimitsPlugin(QueryLimits{MaxFieldCount: 1})

		f := figo.New()
		var err error
		if auditFirst {
			err = f.RegisterPlugin(audit)
			if err == nil {
				err = f.RegisterPlugin(limits)
			}
		} else {
			err = f.RegisterPlugin(limits)
			if err == nil {
				err = f.RegisterPlugin(audit)
			}
		}
		if err != nil {
			t.Fatal(err)
		}

		if err := f.AddFiltersFromString("a=1 and b=2"); err == nil {
			t.Fatal("expected the limits rejection")
		}
		entries := audit.History()
		if len(entries) != 1 || entries[0].Kind != "parse" || entries[0].DSL != "a=1 and b=2" {
			t.Errorf("auditFirst=%v: history = %+v, want exactly one parse-attempt entry", auditFirst, entries)
		}
	}
}
