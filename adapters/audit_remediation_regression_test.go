package adapters

import (
	"regexp"
	"strings"
	"testing"

	. "github.com/bi0dread/figo/v4"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"gorm.io/gorm/clause"
)

// Regression pins for the adapter half of the security audit.

// F2: the preload relation label was the one identifier position handed to
// GORM with no screening at all. GORM RESOLVES it — clause.Associations
// ("~~~as~~~") expands to EVERY association on the schema — and figo's
// per-relation controls are keyed on the label, so a relation figo never named
// cannot have had a mandatory condition registered for it.
func TestF2_PreloadRelationIsScreened(t *testing.T) {
	refused := []string{
		clause.Associations, // gorm's all-associations sentinel
		"~~~as~~~",
		"",
		"Orders..Items",
		"Orders.",
		".Orders",
		"Or ders",
		"Orders;drop",
		"Orders\x00",
		"1Orders", // a relation name cannot start with a digit
		"Or~ders",
	}
	for _, name := range refused {
		assert.Error(t, gormRelationScreen(name), "relation %q must be refused", name)
	}

	accepted := []string{"Orders", "OrderItems", "_private", "Orders2", "Orders.Items", "a.b.c"}
	for _, name := range accepted {
		assert.NoError(t, gormRelationScreen(name), "relation %q must be accepted", name)
	}
}

// F13: gormIdentScreen was added to the filter and projection positions on the
// rationale that raw's validateIdent refuses control bytes and empty
// dot-segments "in EVERY identifier position". The SORT position on GORM was
// not covered, so the two adapters DIVERGED: raw failed the render, GORM
// returned ok=true and unexecutable SQL ("ORDER BY `.`.`.`"), turning an
// available 400 into an engine error at 500.
func TestF13_SortKeyScreenMatchesRawAdapter(t *testing.T) {
	for _, dsl := range []string{
		`age>1 sort=...:asc`,
		"age>1 sort=a\x00b:desc",
		"age>1 sort=a\x01b:asc",
		`age>1 sort=id:asc`, // control: a good key still renders on both
	} {
		t.Run(dsl, func(t *testing.T) {
			fg := New()
			require.NoError(t, fg.AddFiltersFromString(dsl))
			fg.Build(GormAdapter{})
			sort := fg.GetSort()
			require.NotNil(t, sort, "the sort= directive must have parsed")
			_, gormErr := gormOrderByClause(*sort)

			fr := New()
			require.NoError(t, fr.AddFiltersFromString(dsl))
			fr.Build(RawAdapter{})
			_, _, rawErr := AdapterRawGetSql(fr, "t", "SELECT", "FROM", "WHERE", "ORDER BY")

			assert.Equal(t, rawErr == nil, gormErr == nil,
				"raw and gorm must agree on whether this sort key is renderable (raw=%v gorm=%v)", rawErr, gormErr)
		})
	}
}

// F10: Mongo evaluates $regex with PCRE2, where the default '$' matches at the
// end of the subject OR immediately before a final newline — so LIKE 'admin'
// also matched the stored value "admin\n", a row SQL excludes. \A and \z are
// unconditional in PCRE2 and mean what LIKE's implicit anchoring means.
//
// The in-repo semantics oracle evaluates with Go regexp (RE2), where '$'
// without 'm' IS strictly end-of-text — so it is structurally incapable of
// catching a regression here. This pin asserts the emitted PATTERN directly,
// which is the only thing that can.
func TestF10_LikeAnchorsWithAbsoluteBoundaries(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"abc", `\Aabc\z`},
		{"abc%", `\Aabc.*\z`},
		{"%abc%", `\A.*abc.*\z`},
		{"a_c", `\Aa.c\z`},
	} {
		assert.Equal(t, tc.want, likeToRegexPattern(tc.in))
	}

	for _, p := range []string{
		likeToRegexPattern("abc"),
		likeToRegexPattern("%abc%"),
	} {
		assert.NotContains(t, p, "^", "'^' is line-relative under PCRE2's 'm' and newline-permissive at the end")
		assert.NotContains(t, p, "$", "'$' also matches before a trailing newline in PCRE2")
	}
}

// F8: '%' was translated one-for-one to ".*", so a run of N '%' produced N
// ADJACENT unbounded quantifiers — a backtracking bomb on PCRE2 from a handful
// of input bytes. SQL LIKE skips a '%' run in O(1) and never backtracks, so
// collapsing the run is also the faithful rendering.
func TestF8_LikeCollapsesWildcardRuns(t *testing.T) {
	p := likeToRegexPattern(strings.Repeat("%", 20) + "z")
	assert.Equal(t, `\A.*z\z`, p)
	assert.Equal(t, 1, strings.Count(p, ".*"), "a run of '%' must collapse to ONE quantifier")

	// A '_' inside the run still demands one character each, so the collapsed
	// form carries a minimum length rather than losing it.
	assert.Equal(t, `\A.{2,}z\z`, likeToRegexPattern("%_%_%z"))
	assert.Equal(t, `\Aa.*b\z`, likeToRegexPattern("a%%%%b"))
	// A lone '_' is unaffected.
	assert.Equal(t, `\Aa.b\z`, likeToRegexPattern("a_b"))
}

// F7: mongoRawRegex validated the pattern with Go's regexp.Compile and emitted
// it unchanged. Go's regexp is RE2 — no backtracking — so it accepts "^(a+)+$"
// without complaint, while MongoDB evaluates the same pattern with PCRE2, which
// does backtrack: over a second against 28 characters, from 15 bytes of
// untrusted DSL. The compile was never a safety check (it exists because BSON
// serializes a *regexp.Regexp as an empty document); nothing else was one.
func TestF7_RawRegexRefusesNestedUnboundedQuantifiers(t *testing.T) {
	refused := []string{
		`^(a+)+$`,
		`(a*)*`,
		`(a|a)*b`,
		`((a+))+`,
		`(x(y+)z)*`,
		`(a{1,}){2,}`,
		strings.Repeat("a", maxRawRegexPattern+1),
	}
	for _, pat := range refused {
		t.Run("refused/"+shortPat(pat), func(t *testing.T) {
			// RE2 itself is happy with every one of these — that is the point.
			_, ok := mongoRawRegex(pat)
			assert.False(t, ok, "pattern %q must be refused", pat)
		})
	}

	accepted := []string{
		`^admin$`,
		`^user[0-9]+$`,
		`(foo|bar)`,
		`(foo|bar){2}`,
		`^a+$`,
		`[(]+`,    // an unbounded quantifier on a character class, not a group
		`\(a+\)+`, // escaped parens are literals, not a group
		`[a(]*b+`, // '(' inside a class is a literal
	}
	for _, pat := range accepted {
		t.Run("accepted/"+pat, func(t *testing.T) {
			re, ok := mongoRawRegex(pat)
			assert.True(t, ok, "pattern %q must be accepted", pat)
			assert.Equal(t, pat, re.Pattern, "an accepted pattern crosses unchanged")
		})
	}

	// A pattern the APPLICATION compiled is trusted: figo cannot produce one
	// from parsed DSL text, so it is not attacker-reachable.
	_, ok := mongoRawRegex(regexp.MustCompile(`^(a+)+$`))
	assert.True(t, ok, "an application-supplied *regexp.Regexp stays trusted")
}

// F14: rootAfterLookups was a whole-list boolean, so ONE alias-shaped conjunct
// dragged every unrelated conjunct behind the $lookup stages — including a
// mandatory tenant predicate a ScopePlugin injected, which is exactly the
// clause that most wants to run first against an index.
func TestF14_RootMatchIsPartitionedByAliasDependence(t *testing.T) {
	aliases := map[string]bool{"Orders": true}
	exprs := []Expr{
		EqExpr{Field: "tenant_id", Value: 7},
		EqExpr{Field: "Orders.zzz", Value: 1},
		EqExpr{Field: "status", Value: "live"},
	}
	pre, post := partitionByAliasReference(exprs, aliases)
	require.Len(t, pre, 2, "conjuncts naming no alias belong before the joins")
	require.Len(t, post, 1, "only the alias-referencing conjunct moves")
	assert.Equal(t, "Orders.zzz", ExprField(post[0]))

	// Every conjunct is still accounted for: the partition may not drop one.
	assert.Equal(t, len(exprs), len(pre)+len(post))

	// A compound conjunct mixing both goes after — conservative, never wrong.
	mixed := []Expr{OrExpr{Operands: []Expr{
		EqExpr{Field: "tenant_id", Value: 7},
		EqExpr{Field: "Orders.zzz", Value: 1},
	}}}
	pre, post = partitionByAliasReference(mixed, aliases)
	assert.Empty(t, pre)
	assert.Len(t, post, 1)
}

func shortPat(p string) string {
	if len(p) > 20 {
		return p[:20]
	}
	return p
}

var _ = primitive.Regex{}
