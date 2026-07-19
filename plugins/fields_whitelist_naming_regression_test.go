package plugins

import (
	"testing"

	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whitelist must match registered names both verbatim and through the
// instance's naming strategy, exactly like ignore fields do. Previously
// SetAllowedFields("userName") under the default snake_case naming pruned the
// legitimate user_name filter (the whitelist only matched converted names),
// silently widening the query.
func TestWhitelistMatchesNamingConvertedFields(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.SetAllowedFields("userName", "id") // registered in camelCase
	fp.EnableFieldWhitelist()

	f := New()
	require.NoError(t, f.RegisterPlugin(fp))
	require.NoError(t, f.AddFiltersFromString(`userName="x" and id=1 and secret=true`))
	f.Build(RawAdapter{})

	where, _, err := BuildRawWhere(f)
	require.NoError(t, err)
	assert.Contains(t, where, "`user_name` = ?", "allowed camelCase field must survive naming conversion")
	assert.Contains(t, where, "`id` = ?")
	assert.NotContains(t, where, "secret", "non-whitelisted field must still be pruned")
}

// Registering the already-converted spelling keeps working too.
func TestWhitelistMatchesVerbatimFields(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.SetAllowedFields("user_name")
	fp.EnableFieldWhitelist()

	f := New()
	require.NoError(t, f.RegisterPlugin(fp))
	require.NoError(t, f.AddFiltersFromString(`userName="x" and secret=true`))
	f.Build(RawAdapter{})

	where, _, err := BuildRawWhere(f)
	require.NoError(t, err)
	assert.Contains(t, where, "`user_name` = ?")
	assert.NotContains(t, where, "secret")
}
