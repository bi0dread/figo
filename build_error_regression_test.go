package figo_test

import (
	"fmt"
	"strings"
	"testing"

	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// BuildE must surface everything the parser silently drops, while building
// exactly the same clause tree Build does.

func TestBuildEValidDSLReturnsNil(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`id=1 and (age>20 or active=true) and name<in>["a","b"] sort=id:desc page=skip:0,take:10 load=[Rel:x=1]`))
	assert.NoError(t, f.BuildE(RawAdapter{}))
}

func TestBuildEReportsUnrecognizedTokens(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`id=1 zzz`))
	err := f.BuildE(RawAdapter{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized token")
	assert.Contains(t, err.Error(), `"zzz"`)

	// The valid part still builds, identically to Build.
	where, args, werr := BuildRawWhere(f)
	require.NoError(t, werr)
	assert.Contains(t, where, "`id` = ?")
	assert.Equal(t, []any{int64(1)}, args)
}

func TestBuildEReportsInvalidBetweenValue(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`price<bet>(10)`)) // no ".." separator
	err := f.BuildE(RawAdapter{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid value")
	assert.Contains(t, err.Error(), "<bet>")
}

func TestBuildEReportsMalformedDirectives(t *testing.T) {
	cases := []struct {
		name string
		dsl  string
		want string
	}{
		{"PageNonInteger", `id=1 page=skip:abc`, "invalid page= value"},
		{"PageUnknownKey", `id=1 page=foo:3`, "unknown page= key"},
		{"SortMissingDirection", `id=1 sort=name`, "malformed sort= segment"},
		{"SortBadDirection", `id=1 sort=name:down`, "invalid sort direction"},
		{"LoadMissingColon", `id=1 load=[Rel x=1]`, "malformed load= segment"},
		{"LoadUnclosed", `id=1 load=[Rel:x=1`, "unclosed load= directive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := New()
			require.NoError(t, f.AddFiltersFromString(tc.dsl))
			err := f.BuildE(RawAdapter{})
			require.Error(t, err, "dsl: %s", tc.dsl)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestBuildStaysSilentAndIdenticalToBuildE(t *testing.T) {
	dsl := `id=1 zzz and age>5`

	f1 := New()
	require.NoError(t, f1.AddFiltersFromString(dsl))
	f1.Build(RawAdapter{}) // must not panic; errors discarded

	f2 := New()
	require.NoError(t, f2.AddFiltersFromString(dsl))
	require.Error(t, f2.BuildE(RawAdapter{}))

	// Identical clause trees regardless of which entry point ran.
	assert.Equal(t, f1.Explain(), f2.Explain())
}

// afterParseRejector rejects any DSL containing "reject" — a stand-in for
// LimitsPlugin/ValidationPlugin, whose AfterParse errors must not leave the
// rejected DSL armed for a later Build.
type afterParseRejector struct{}

func (afterParseRejector) Name() string                                   { return "after-parse-rejector" }
func (afterParseRejector) Version() string                                { return "1.0.0" }
func (afterParseRejector) Initialize(Figo) error                          { return nil }
func (afterParseRejector) BeforeQuery(Figo, any) error                    { return nil }
func (afterParseRejector) AfterQuery(Figo, any, interface{}) error        { return nil }
func (afterParseRejector) BeforeParse(_ Figo, dsl string) (string, error) { return dsl, nil }
func (afterParseRejector) AfterParse(_ Figo, dsl string) error {
	if strings.Contains(dsl, "reject") {
		return fmt.Errorf("rejected by policy")
	}
	return nil
}

func TestAfterParseErrorRollsBackDSL(t *testing.T) {
	f := New()
	require.NoError(t, f.RegisterPlugin(afterParseRejector{}))

	require.NoError(t, f.AddFiltersFromString(`id=1`))
	require.Error(t, f.AddFiltersFromString(`reject_me=1`))

	// The rejected DSL must not have replaced the accepted one.
	assert.Equal(t, `id=1`, f.GetDSL())

	// A caller that ignores the error and Builds anyway gets the LAST
	// ACCEPTED query, not the rejected one.
	f.Build(RawAdapter{})
	where, _, err := BuildRawWhere(f)
	require.NoError(t, err)
	assert.Contains(t, where, "`id` = ?")
	assert.NotContains(t, where, "reject_me")
}

func TestAfterParseErrorOnFirstCallLeavesDSLEmpty(t *testing.T) {
	f := New()
	require.NoError(t, f.RegisterPlugin(afterParseRejector{}))

	require.Error(t, f.AddFiltersFromString(`reject_me=1`))
	assert.Equal(t, "", f.GetDSL())

	f.Build(RawAdapter{})
	assert.Empty(t, f.GetClauses())
}
