package figo

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression pins for the core-parser half of the security audit. Each test
// names the finding it pins and, where an EXISTING test covered the same bug in
// a narrower spelling, says which one and why it did not catch this.

// F1: a dotted load= label is a PATH through the association graph, not a
// longer relation name. Refused unless the instance opts in, because every
// per-relation control (notably ScopePlugin.AddPreloadScope) is keyed on the
// exact label, so a path walks straight past them.
func TestF1_DottedPreloadLabelRefusedByDefault(t *testing.T) {
	f := New()
	err := f.BuildE(nil)
	require.NoError(t, err)

	f = New()
	require.NoError(t, f.AddFiltersFromString(`age>1 load=[Orders.Items:]`))
	err = f.BuildE(nil)
	require.Error(t, err, "a nested relation path must be diagnosed")
	assert.Contains(t, err.Error(), "nested preloads are disabled")
	assert.Empty(t, f.GetPreloads(), "the relation must not be preloaded at all: dropping it is the only choice that cannot widen")

	// The refusal must be a REJECTION, not an informational note — otherwise
	// the InjectionGuardPlugin would let it through (see F5).
	for _, part := range DiagParts(err) {
		if strings.Contains(part.Error(), "nested preloads are disabled") {
			assert.False(t, IsInformationalDiag(part), "the refusal must be destructive")
		}
	}

	// Opted in on a PATH adapter, every PREFIX is registered as a relation of
	// its own so a PreloadFinalizer can attach conditions to the intermediate
	// query the adapter issues implicitly.
	f = New()
	f.SetAllowNestedPreloads(true)
	require.NoError(t, f.AddFiltersFromString(`age>1 load=[Orders.Items:]`))
	_ = f.BuildE(pathExpandingAdapter{})
	pre := f.GetPreloads()
	assert.Contains(t, pre, "Orders.Items")
	assert.Contains(t, pre, "Orders", "the prefix must be a relation in its own right, or nothing can scope it")

	// On an adapter that does NOT treat the label as a path, the map is left
	// exactly as parsed: Mongo resolves a label against its configured joins
	// (a synthesized prefix would fail the build) and raw hands the map to the
	// caller (who would issue an unfiltered query for a relation the DSL never
	// named).
	f = New()
	f.SetAllowNestedPreloads(true)
	require.NoError(t, f.AddFiltersFromString(`age>1 load=[Orders.Items:]`))
	_ = f.BuildE(nil)
	pre = f.GetPreloads()
	assert.Contains(t, pre, "Orders.Items")
	assert.NotContains(t, pre, "Orders", "a non-path adapter must not gain a synthesized relation")

	// The path length is bounded even with nesting on: the depth is
	// attacker-chosen and each segment costs a map entry and a query.
	f = New()
	f.SetAllowNestedPreloads(true)
	deep := "A"
	for i := 0; i < maxPreloadPathSegments+2; i++ {
		deep += ".B"
	}
	_ = f.AddFiltersFromString(`age>1 load=[` + deep + `:]`)
	err = f.BuildE(pathExpandingAdapter{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nests more than")
	assert.Empty(t, f.GetPreloads())
}

// pathExpandingAdapter stands in for the GORM adapter, which is the only one
// that treats a dotted label as a path (see NestedPreloadPathExpander). The
// core test package cannot import adapters (they import it).
type pathExpandingAdapter struct{}

func (pathExpandingAdapter) ExpandsNestedPreloadPaths() bool { return true }
func (pathExpandingAdapter) GetSqlString(Figo, any, ...string) (string, bool) {
	return "", false
}
func (pathExpandingAdapter) GetQuery(Figo, any, ...string) (Query, bool) { return nil, false }

// F3: absorbAroundNonOperand's forward rescan was O(N^2) in the number of
// consecutive non-operand slots. Every shape below is a run of them, and none
// is visible to dslResourceGuard: the byte cap is satisfied and "()()()" never
// nests deeper than one level.
//
// Not covered by TestDeepGroupNestingParsesQuickly or the flat-connector
// complexity tests: their inputs are all OPERANDS, so the rescan is never
// reached.
func TestF3_NonOperandRunsParseLinearly(t *testing.T) {
	for _, unit := range []string{"()", "sort= ", "page= "} {
		unit := unit
		t.Run(strings.TrimSpace(unit), func(t *testing.T) {
			dsl := strings.Repeat(unit, 100000)
			require.LessOrEqual(t, len(dsl), maxDSLBytes, "the pin must stay inside the parser cap")

			f := New()
			require.NoError(t, f.AddFiltersFromString(dsl))
			done := make(chan struct{})
			go func() { _ = f.BuildE(nil); close(done) }()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatalf("parsing %d %q slots did not finish: the quadratic rescan is back", 100000, unit)
			}
		})
	}
}

// F3, the same defect at the unit level: the precomputed table must agree with
// the forward scan it replaced, for every position and every child shape.
func TestF3_NextItemKindsMatchesForwardScan(t *testing.T) {
	children := []*Node{
		{Operator: operationDirective, Value: "sort=id:asc"},
		{Operator: OperationChild},
		{Expression: []Expr{EqExpr{Field: "a", Value: 1}}},
		{Operator: OperationAnd},
		{Operator: operationDirective, Value: "page=skip:0,take:5"},
		{Operator: OperationNot},
		{Operator: OperationChild},
		{Expression: []Expr{EqExpr{Field: "b", Value: 2}}},
		{Operator: OperationOr},
		{Operator: OperationChild},
	}
	kinds, ops := nextItemKinds(children)
	for i := 0; i <= len(children); i++ {
		wantKind, wantOp := nextItemKind(children, i)
		assert.Equal(t, wantKind, kinds[i], "kind disagrees at %d", i)
		assert.Equal(t, wantOp, ops[i], "op disagrees at %d", i)
	}
}

// F9: parseScalarLiteral tries parseDate before falling back to string, so a
// date-shaped bound is a time.Time — neither numeric nor string, which is how a
// number..date pair passed a guard whose whole job was mixed bounds. SQLite,
// MySQL and BSON all order every number below every date, so the ceiling
// evaporated.
func TestF9_BetweenRefusesMixedBoundKinds(t *testing.T) {
	refused := []string{
		`price<bet>(10..2024-01-02)`,
		`price<bet>(2024-01-02..10)`,
		`price<bet>(10..abc)`,
		`price<bet>(abc..10)`,
		`price<bet>(10..true)`,
	}
	for _, dsl := range refused {
		t.Run("refused/"+dsl, func(t *testing.T) {
			f := New()
			_ = f.AddFiltersFromString(dsl)
			_ = f.BuildE(nil)
			for _, c := range f.GetClauses() {
				assert.NotContains(t, exprKindsIn(c), "BetweenExpr",
					"mixed-kind bounds must not render a BETWEEN")
			}
		})
	}

	accepted := []string{
		`price<bet>(10..20)`,
		`created<bet>(2024-01-02..2024-03-04)`,
		// The `null` sentinel is exempt: it is neither bound kind, it was
		// always admitted, and the ES truth-table oracle pins its rendering.
		`price<bet>(null..2)`,
		`price<bet>(2..null)`,
	}
	for _, dsl := range accepted {
		t.Run("accepted/"+dsl, func(t *testing.T) {
			f := New()
			_ = f.AddFiltersFromString(dsl)
			_ = f.BuildE(nil)
			found := false
			for _, c := range f.GetClauses() {
				if strings.Contains(exprKindsIn(c), "BetweenExpr") {
					found = true
				}
			}
			assert.True(t, found, "a same-kind range must still build")
		})
	}
}

func exprKindsIn(e Expr) string {
	var b strings.Builder
	Walk(e, func(n Expr) {
		// Walk visits with a POINTER to each node (see walk.go).
		switch n.(type) {
		case *BetweenExpr:
			b.WriteString("BetweenExpr ")
		}
	})
	return b.String()
}

// F15: dslResourceGuard tracked quote state with ONE global toggle over the
// whole string, while the tokenizer tracks it per token and the load= branch
// rewinds past the directive's ']'. An unterminated quote is where the two
// models part company — everything after it was "quoted" to the guard, so the
// nesting cap saw depth 0.
//
// Commit 65ad558 states the depth scan is "quote-aware under the tokenizer's
// own rule"; this is the input that made that false.
func TestF15_DepthCapSurvivesUnterminatedQuote(t *testing.T) {
	deep := strings.Repeat("(", maxDSLGroupDepth+1)

	assert.NotEmpty(t, dslResourceGuard(deep), "the plain form must be capped")
	assert.NotEmpty(t, dslResourceGuard(`load=[X:a="`+deep+`a=1`),
		"an unterminated quote must not hide the nesting from the cap")
	assert.NotEmpty(t, dslResourceGuard(`name="`+deep),
		"the bare unterminated-quote form too")

	// A well-formed DSL is judged exactly as before: parens inside a CLOSED
	// string literal are data, not nesting.
	assert.Empty(t, dslResourceGuard(`name="`+strings.Repeat("(", 100)+`" and a=1`))
	assert.Empty(t, dslResourceGuard(strings.Repeat("(", 10)+"a=1"+strings.Repeat(")", 10)))
}

// F4: the tokenizer left the token at the ')' of a load= filter WITHOUT
// decrementing bracketDepth, so the load= branch saw an unbalanced bracket and
// fell back to the rescan meant for a token it could not close. That rescan is
// quote-blind: it stopped at the ']' inside a string literal, parsing resumed
// mid-literal, and every following filter was absorbed into one discarded token
// — rendering the statement with NO WHERE at all.
//
// TestCORE1_LoadDirectiveEndIsQuoteAware pins the same failure, but only for
// load= filters containing no parenthesis, so the paren-exit path was unpinned.
func TestF4_LoadFilterWithParenKeepsFollowingFilters(t *testing.T) {
	for _, dsl := range []string{
		`load=[Orders:name="a]b"] and tenant_id=7`,
		`load=[Orders:(id=1) and name="a]b"] and tenant_id=7`,
		`load=[Orders:(id=1) or (id=2) and name="a]b"] and tenant_id=7`,
		`load=[Orders:total<bet>(1..2) and n="]"] and tenant_id=7`,
		`load=[Orders:(id=1) and name="ab"] and tenant_id=7`,
	} {
		t.Run(dsl, func(t *testing.T) {
			f := New()
			_ = f.AddFiltersFromString(dsl)
			_ = f.BuildE(nil)

			found := false
			for _, c := range f.GetClauses() {
				Walk(c, func(n Expr) {
					if e, ok := n.(*EqExpr); ok && e.Field == "tenant_id" {
						found = true
					}
				})
			}
			assert.True(t, found,
				"the filter written after the load= directive was swallowed; the statement would render unfiltered")
		})
	}
}

// F5: the informational/destructive classification travels as a Go TYPE,
// because every diagnostic interpolates the caller's own input and a consumer
// matching the MESSAGE was matching attacker-controlled bytes.
func TestF5_DiagnosticClassificationIsNotForgeable(t *testing.T) {
	// Genuinely informational: the built query still expresses everything.
	for _, dsl := range []string{`id<in>[1,2,]`, `id=1 load=[Orders:]`} {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		_ = f.AddFiltersFromString(dsl)
		parts := DiagParts(f.BuildE(nil))
		require.NotEmpty(t, parts)
		for _, p := range parts {
			assert.True(t, IsInformationalDiag(p), "%q: expected informational, got %v", dsl, p)
		}
	}

	// The same phrases, supplied by the caller inside a DESTRUCTIVE diagnostic,
	// must not launder it. This is the whole finding.
	for _, dsl := range []string{
		`load=[X:empty element in list`,
		`name="empty element in list and tenant_id=42`,
		`load=[X:produced no conditions; relation preloaded unfiltered`,
	} {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		_ = f.AddFiltersFromString(dsl)
		parts := DiagParts(f.BuildE(nil))
		require.NotEmpty(t, parts, "%q must diagnose", dsl)
		destructive := false
		for _, p := range parts {
			if !IsInformationalDiag(p) {
				destructive = true
			}
		}
		assert.True(t, destructive, "%q laundered its rejection into an informational note", dsl)
	}
}

// DiagParts must flatten nested joins and never lose a diagnostic, since the
// guard now iterates it instead of splitting text on newlines.
func TestF5_DiagPartsFlattensJoins(t *testing.T) {
	assert.Nil(t, DiagParts(nil))

	f := New()
	_ = f.AddFiltersFromString(`load=[X: and )`)
	err := f.BuildE(nil)
	require.Error(t, err)
	parts := DiagParts(err)
	require.NotEmpty(t, parts)
	for _, p := range parts {
		assert.NotContains(t, p.Error(), "\n", "each part must be one diagnostic, not a joined blob")
	}
}

// ---------------------------------------------------------------------------
// Round 2: pins for defects found in the fixes themselves.
// ---------------------------------------------------------------------------

// The F4 relaxation must apply ONLY to a load= bracket. Relaxing it for every
// bracket removed the last rule that could terminate a token inside an
// UNCLOSED '[', so the token ran to end of input and every following conjunct
// was absorbed into one list member — the same fail-open F5 closes, arriving
// through the tokenizer.
func TestF4_UnclosedListBracketStillTerminates(t *testing.T) {
	for _, dsl := range []string{
		`status<nin>[archived) and tenant_id=7`,
		`status<nin>[(archived) and tenant_id=7`,
		`id<in>[(1,2) and tenant_id=7`,
		`id<in>[1,2] and tenant_id=7`,
		`price<bet>(1..2) and tenant_id=7`,
	} {
		t.Run(dsl, func(t *testing.T) {
			f := New()
			_ = f.AddFiltersFromString(dsl)
			_ = f.BuildE(nil)
			found := false
			for _, c := range f.GetClauses() {
				Walk(c, func(n Expr) {
					if e, ok := n.(*EqExpr); ok && e.Field == "tenant_id" {
						found = true
					}
				})
			}
			assert.True(t, found, "an unclosed or paren-bearing list bracket swallowed the rest of the DSL")
		})
	}
}

// The prefix expansion must be LINEAR in the label length. strings.Join
// allocated a fresh string per prefix, so a label of N segments cost O(N^2) in
// time and in RETAINED heap — 128 KB of DSL held 4.3 GB in the preload map,
// under both pre-parse caps and before any plugin could measure it.
func TestF1_PrefixExpansionIsBounded(t *testing.T) {
	f := New()
	f.SetAllowNestedPreloads(true)
	deep := "A" + strings.Repeat(".B", 64000)
	_ = f.AddFiltersFromString(`load=[` + deep + `:]`)

	done := make(chan struct{})
	go func() { _ = f.BuildE(pathExpandingAdapter{}); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("prefix expansion did not finish: the quadratic join is back")
	}
	// The depth cap refuses it outright, so nothing is registered at all.
	assert.Empty(t, f.GetPreloads(), "a path past the depth cap must not be preloaded")
}

// IsInformationalDiag must be safe on the JOINED error BuildE returns. errors.As
// traverses a join tree and matches on any branch, so the natural spelling
// `if err != nil && !IsInformationalDiag(err) { reject }` was forgeable by
// prepending a harmless informational construct to a destructive query.
func TestF5_IsInformationalDiagRequiresEveryPart(t *testing.T) {
	f := New()
	f.SetNamingFunc(NoChangeNaming)
	_ = f.AddFiltersFromString(`id<in>[1,2,] and garbagetoken`)
	err := f.BuildE(nil)
	require.Error(t, err)

	parts := DiagParts(err)
	require.Greater(t, len(parts), 1, "this DSL must produce a joined diagnostic")
	anyInfo := false
	for _, p := range parts {
		if IsInformationalDiag(p) {
			anyInfo = true
		}
	}
	require.True(t, anyInfo, "one part must be informational for this to test anything")
	assert.False(t, IsInformationalDiag(err),
		"a join is informational only when EVERY part is")

	// An all-informational join still reports informational.
	f = New()
	f.SetNamingFunc(NoChangeNaming)
	_ = f.AddFiltersFromString(`id<in>[1,2,] and x=1 load=[Orders:]`)
	assert.True(t, IsInformationalDiag(f.BuildE(nil)))
}

// A list whose members are ALL blank is not a smaller filter but a different
// one: <nin> renders 1=1, i.e. it matches everything. Classifying that as
// informational told a validating caller nothing had changed.
func TestF5_AllBlankListIsARejection(t *testing.T) {
	f := New()
	f.SetNamingFunc(NoChangeNaming)
	_ = f.AddFiltersFromString(`role<nin>[,]`)
	err := f.BuildE(nil)
	require.Error(t, err)
	assert.False(t, IsInformationalDiag(err),
		"an emptied list widens the predicate, so it is a rejection")

	// A blank element among real ones is still just a separator artifact.
	f = New()
	f.SetNamingFunc(NoChangeNaming)
	_ = f.AddFiltersFromString(`role<nin>[a,,b]`)
	assert.True(t, IsInformationalDiag(f.BuildE(nil)))
}

// DiagParts must not recurse without bound: a caller folding BuildE's error
// into their own cyclic multi-error type would otherwise take down the process
// with an unrecoverable stack overflow.
func TestF5_DiagPartsIsDepthBounded(t *testing.T) {
	cyc := &cyclicErr{}
	cyc.parts = []error{cyc}
	done := make(chan int, 1)
	go func() { done <- len(DiagParts(cyc)) }()
	select {
	case n := <-done:
		assert.Positive(t, n, "a bounded flatten must still return the error")
	case <-time.After(5 * time.Second):
		t.Fatal("DiagParts did not terminate on a cyclic error")
	}
}

type cyclicErr struct{ parts []error }

func (c *cyclicErr) Error() string   { return "cyclic" }
func (c *cyclicErr) Unwrap() []error { return c.parts }
