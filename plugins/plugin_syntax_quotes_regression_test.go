package plugins

import (
	"errors"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The structural validators counted ()[] inside double-quoted values, so
// strict mode rejected DSL the core parser handles fine — and repair mode
// EDITED the quoted value (name="a)b" became name="ab", silently querying a
// different string).
func TestSyntaxPluginIsQuoteAware(t *testing.T) {
	valid := []string{
		`name="a)b"`,
		`name="a(b"`,
		`name="a[b"`,
		`name="a]b"`,
		`note="(unbalanced] everywhere)" and id=1`,
	}

	t.Run("strict accepts quoted specials", func(t *testing.T) {
		for _, dsl := range valid {
			f := figo.New()
			require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(false)))
			assert.NoError(t, f.AddFiltersFromString(dsl), "dsl: %s", dsl)
		}
	})

	t.Run("repair leaves quoted values intact", func(t *testing.T) {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(true)))
		require.NoError(t, f.AddFiltersFromString(`name="a)b"`))
		assert.Equal(t, `name="a)b"`, f.GetDSL(), "repair must not edit inside a quoted value")

		f.Build(adapters.RawAdapter{})
		_, args, err := adapters.BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, []any{"a)b"}, args)
	})

	t.Run("strict still catches real imbalance", func(t *testing.T) {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(false)))
		err := f.AddFiltersFromString(`(name="x" and (age>5)`)
		require.Error(t, err)
		var pe *figo.ParseError
		assert.True(t, errors.As(err, &pe))
	})
}

// Doubled equality operators parse as a predicate on an empty field name and
// used to pass strict validation silently (the README documented rejection
// that did not exist).
func TestSyntaxPluginRejectsDoubledOperators(t *testing.T) {
	bad := []string{
		`name = = 5`,
		`name==5`,
		`name===5`,
	}
	for _, dsl := range bad {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(false)))
		err := f.AddFiltersFromString(dsl)
		require.Error(t, err, "dsl: %s", dsl)
		var pe *figo.ParseError
		require.True(t, errors.As(err, &pe), "dsl: %s", dsl)
		assert.Contains(t, pe.Message, "doubled equality")
	}

	good := []string{
		`a>=5`,
		`a<=5`,
		`a!=5`,
		`a=^"%x%"`,
		`email=~"x==y"`,      // quoted value: '=' is literal
		`token="=="`,         // ditto
		`pattern=~[a-z]+==$`, // unquoted regex VALUE: '=' not in operator position
	}
	for _, dsl := range good {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(false)))
		assert.NoError(t, f.AddFiltersFromString(dsl), "dsl: %s", dsl)
	}
}

// Trailing multi-char operators report their own message instead of the bare
// '=' one (`a >=` used to match the `=$` pattern first).
func TestSyntaxPluginTrailingOperatorMessages(t *testing.T) {
	f := figo.New()
	require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(false)))
	err := f.AddFiltersFromString(`a >=`)
	require.Error(t, err)
	var pe *figo.ParseError
	require.True(t, errors.As(err, &pe))
	assert.Contains(t, pe.Message, "greater than or equal")
}

// The documented repair example keeps working, with quotes fixed before
// parens so the appended ')' lands outside the closed string.
func TestSyntaxPluginRepairOrderQuotesFirst(t *testing.T) {
	f := figo.New()
	require.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(true)))
	require.NoError(t, f.AddFiltersFromString(`(name="john and age>25`))
	assert.Equal(t, `(name="john and age>25")`, f.GetDSL())
}
