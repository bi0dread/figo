package plugins

import (
	"strings"
	"sync"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rules must match through the instance's naming strategy, exactly like
// FieldsPlugin's ignore/whitelist matching: the parser converts userEmail to
// user_email BEFORE validation sees it, so a rule registered with the
// camelCase spelling silently never fired — invalid values passed unchecked.
func TestValidationRuleMatchesThroughNamingStrategy(t *testing.T) {
	newF := func() (figo.Figo, *ValidationPlugin) {
		f := figo.New() // default SnakeCaseNaming
		vp := NewValidationPlugin()
		vp.RegisterValidator(EmailValidator{})
		vp.AddRule(ValidationRule{Field: "userEmail", Rule: "email", Message: "invalid email"})
		require.NoError(t, f.RegisterPlugin(vp))
		return f, vp
	}

	t.Run("camelCase rule fires on snake_case parsed field", func(t *testing.T) {
		f, _ := newF()
		err := f.AddFiltersFromString(`userEmail="not-an-email"`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid email")
	})

	t.Run("valid value still passes", func(t *testing.T) {
		f, _ := newF()
		require.NoError(t, f.AddFiltersFromString(`userEmail="a@b.com"`))
	})

	t.Run("snake_case registration keeps working", func(t *testing.T) {
		f := figo.New()
		vp := NewValidationPlugin()
		vp.RegisterValidator(EmailValidator{})
		vp.AddRule(ValidationRule{Field: "user_email", Rule: "email", Message: "invalid email"})
		require.NoError(t, f.RegisterPlugin(vp))
		err := f.AddFiltersFromString(`userEmail="nope"`)
		require.Error(t, err)
	})
}

// A rule naming a validator that was never registered used to validate
// NOTHING silently — a typo in the Rule string switched the check off.
func TestValidationUnknownRuleNameFailsClosed(t *testing.T) {
	f := figo.New()
	vp := NewValidationPlugin()
	vp.AddRule(ValidationRule{Field: "name", Rule: "emial" /* typo, never registered */})
	require.NoError(t, f.RegisterPlugin(vp))

	err := f.AddFiltersFromString(`name="x"`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no registered validator")
}

// An empty Message used to produce "validation failed for field x: " with the
// underlying cause discarded; the validator's own error surfaces now.
func TestValidationEmptyMessageSurfacesValidatorError(t *testing.T) {
	f := figo.New()
	vp := NewValidationPlugin()
	vp.RegisterValidator(EmailValidator{})
	vp.AddRule(ValidationRule{Field: "email", Rule: "email"}) // no Message
	require.NoError(t, f.RegisterPlugin(vp))

	err := f.AddFiltersFromString(`email="nope"`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a valid email")
}

// orderProbePlugin records the order its BeforeParse hook runs in.
type orderProbePlugin struct {
	name string
	mu   *sync.Mutex
	seen *[]string
}

func (p orderProbePlugin) Name() string                         { return p.name }
func (p orderProbePlugin) Version() string                      { return "1.0.0" }
func (p orderProbePlugin) Initialize(figo.Figo) error           { return nil }
func (p orderProbePlugin) BeforeQuery(figo.Figo, any) error     { return nil }
func (p orderProbePlugin) AfterQuery(figo.Figo, any, any) error { return nil }
func (p orderProbePlugin) AfterParse(figo.Figo, string) error   { return nil }
func (p orderProbePlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) {
	p.mu.Lock()
	*p.seen = append(*p.seen, p.name)
	p.mu.Unlock()
	return dsl, nil
}

// Hook dispatch iterated a Go map, so plugin order was random per call — two
// DSL-rewriting plugins composed differently from one AddFiltersFromString to
// the next. Hooks now run in registration order, every time.
func TestPluginHooksRunInRegistrationOrder(t *testing.T) {
	f := figo.New()
	var mu sync.Mutex
	var seen []string
	for _, name := range []string{"p1", "p2", "p3"} {
		require.NoError(t, f.RegisterPlugin(orderProbePlugin{name: name, mu: &mu, seen: &seen}))
	}

	for i := 0; i < 50; i++ {
		seen = seen[:0]
		require.NoError(t, f.AddFiltersFromString(`a=1`))
		assert.Equal(t, []string{"p1", "p2", "p3"}, seen, "iteration %d", i)
	}
}

// Depth counts logical nesting as read, not parser tree levels: the parser
// builds flat and-chains as left-nested binary AndExprs, so an 11-term FLAT
// query used to measure depth 11 and trip the default MaxNestingDepth of 10.
func TestNestingDepthCountsLogicalLevelsNotTreeLevels(t *testing.T) {
	newLimited := func(depth int) figo.Figo {
		f := figo.New()
		lp := NewLimitsPlugin(QueryLimits{MaxNestingDepth: depth})
		require.NoError(t, f.RegisterPlugin(lp))
		return f
	}

	t.Run("flat 11-term and-chain is depth 1", func(t *testing.T) {
		f := newLimited(1)
		parts := make([]string, 11)
		for i := range parts {
			parts[i] = "f" + string(rune('a'+i)) + "=1"
		}
		require.NoError(t, f.AddFiltersFromString(strings.Join(parts, " and ")))
	})

	t.Run("one nested group of a different connector is depth 2", func(t *testing.T) {
		f := newLimited(1)
		err := f.AddFiltersFromString(`a=1 and (b=2 or c=3)`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MaxNestingDepth: 2 > 1")

		f2 := newLimited(2)
		require.NoError(t, f2.AddFiltersFromString(`a=1 and (b=2 or c=3)`))
	})

	t.Run("deeper alternation still measured", func(t *testing.T) {
		f := newLimited(2)
		err := f.AddFiltersFromString(`a=1 and (b=2 or (c=3 and d=4))`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MaxNestingDepth: 3 > 2")
	})
}
