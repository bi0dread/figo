package plugins

import (
	"errors"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parserValidButHeuristicFlagged are DSL strings the core parser accepts
// exactly as written but the heuristic validators used to flag: `x<null>` /
// `x<notnull>` end in '>' (read as a trailing greater-than), a base64 value
// ends in '=', a possessive apostrophe read as an unmatched quote, and '=='
// inside an unquoted regex value read as a doubled operator.
var parserValidButHeuristicFlagged = []string{
	`x<null>`,
	`x<notnull>`,
	`data=aGVsbG8=`,
	`name=O'Brien`,
	`email=~x==y`,
}

// B6 (strict): parser-valid input must not be rejected.
func TestSyntaxStrictAcceptsParserValidInput(t *testing.T) {
	for _, dsl := range parserValidButHeuristicFlagged {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(false)))
		require.NoError(t, f.AddFiltersFromString(dsl), "dsl: %s", dsl)
		assert.Equal(t, dsl, f.GetDSL(), "dsl must pass through unchanged: %s", dsl)
	}
}

// B6 (repair): parser-valid input must not be "repaired" into a different
// query. Repairing `x<null>` used to yield a DSL whose WHERE rendered empty
// (the filter silently vanished) and `name=O'Brien` got a quote appended,
// querying the literal value `O'Brien'`.
func TestSyntaxRepairPassesParserValidInputUnchanged(t *testing.T) {
	for _, dsl := range parserValidButHeuristicFlagged {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(true)))
		require.NoError(t, f.AddFiltersFromString(dsl), "dsl: %s", dsl)
		assert.Equal(t, dsl, f.GetDSL(), "repair must not touch parser-valid dsl: %s", dsl)
	}

	t.Run("null filter renders instead of vanishing", func(t *testing.T) {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(true)))
		require.NoError(t, f.AddFiltersFromString(`x<null>`))
		f.Build(adapters.RawAdapter{})
		where, _, err := adapters.BuildRawWhere(f)
		require.NoError(t, err)
		assert.Contains(t, where, "IS NULL", "the user's filter must not silently vanish")
	})

	t.Run("apostrophe value queried literally", func(t *testing.T) {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(true)))
		require.NoError(t, f.AddFiltersFromString(`name=O'Brien`))
		f.Build(adapters.RawAdapter{})
		_, args, err := adapters.BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, []any{"O'Brien"}, args, "repair must not append a quote to the value")
	})
}

// B6 (repair): repair must never delete characters inside an unclosed quoted
// region — the quote is closed at the end instead. The trailing-connector
// rewrite used to strip " and" out of the value before the quote was closed.
func TestSyntaxRepairKeepsTextInsideUnclosedQuote(t *testing.T) {
	f := figo.New()
	require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(true)))
	require.NoError(t, f.AddFiltersFromString(`name="foo and`))
	assert.Equal(t, `name="foo and"`, f.GetDSL())

	f.Build(adapters.RawAdapter{})
	_, args, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, []any{"foo and"}, args, "text inside the unclosed quote must survive repair")
}

// B6 regression guard: genuinely-broken input must STILL be rejected in
// strict mode — the parse-validity gate inspects the parsed tree, because the
// permissive parser "accepts" most of these with zero diagnostics (`a==b`
// parses into Eq(a, "=b"), `name="x` folds the raw quote into the value,
// `a >=` yields an empty value, a dangling `and` is silently dropped).
func TestSyntaxGenuinelyBrokenStillRejected(t *testing.T) {
	broken := []string{
		`a==b`,
		`name==5`,
		`name = = 5`,
		`name===5`,
		`name="x`,
		`name="foo and`,
		`id=1 and`,
		`a=1 or`,
		`a >=`,
		`name=^`,
		`name!=^`,
		`(name="x" and (age>5)`,
	}
	for _, dsl := range broken {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(false)))
		err := f.AddFiltersFromString(dsl)
		require.Error(t, err, "dsl must stay rejected: %s", dsl)
		var pe *figo.ParseError
		assert.True(t, errors.As(err, &pe), "dsl: %s", dsl)
	}
}
