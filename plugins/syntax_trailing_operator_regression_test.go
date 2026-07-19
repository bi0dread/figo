package plugins

import (
	. "github.com/bi0dread/figo/v4"

	"testing"

	"github.com/stretchr/testify/assert"
)

// The trailing-operator validation patterns for `=^` / `!=^` contained an
// unescaped `^` — a start-of-text anchor mid-pattern, so they could never
// match and a DSL ending in an incomplete LIKE operator slipped through the
// strict syntax plugin instead of being rejected.
func TestSyntaxRejectsTrailingLikeOperator(t *testing.T) {
	for _, dsl := range []string{`name=^`, `name!=^`} {
		f := New()
		assert.NoError(t, f.RegisterPlugin(NewSyntaxPlugin(false)))
		err := f.AddFiltersFromString(dsl)
		assert.Error(t, err, "trailing %q must be rejected", dsl)
	}
}
