package plugins

import (
	figo "github.com/bi0dread/figo/v4"
)

import (
	"fmt"
	"strings"
	"sync"
)

// Validation is provided as a plugin rather than core figo state: register a
// ValidationPlugin on an instance and every AddFiltersFromString call is
// validated automatically, or use the plugin standalone via Validate.

// ValidationRule represents a validation rule
type ValidationRule struct {
	Field   string
	Rule    string
	Value   any
	Message string
	Handler func(field, rule string, value any) error
}

// Validator interface for custom validation
type Validator interface {
	Validate(field, rule string, value any) error
	GetRuleName() string
}

// ValidationPlugin validates filter values against registered rules. It
// implements Plugin: once registered (f.RegisterPlugin), its AfterParse hook
// parses the DSL and validates every field's values, failing the
// AddFiltersFromString call on the first violation. Validate can also be
// called directly for one-off checks.
type ValidationPlugin struct {
	rules      []ValidationRule
	validators map[string]Validator
	mu         sync.RWMutex
}

// NewValidationPlugin creates a new validation plugin
func NewValidationPlugin() *ValidationPlugin {
	return &ValidationPlugin{
		rules:      make([]ValidationRule, 0),
		validators: make(map[string]Validator),
	}
}

// Name implements Plugin
func (p *ValidationPlugin) Name() string { return "figo-validation" }

// Version implements Plugin
func (p *ValidationPlugin) Version() string { return "1.0.0" }

// Initialize implements Plugin
func (p *ValidationPlugin) Initialize(figo.Figo) error { return nil }

// BeforeQuery implements Plugin
func (p *ValidationPlugin) BeforeQuery(figo.Figo, any) error { return nil }

// AfterQuery implements Plugin
func (p *ValidationPlugin) AfterQuery(figo.Figo, any, any) error { return nil }

// BeforeParse implements Plugin
func (p *ValidationPlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) { return dsl, nil }

// AfterParse validates every filter value in the freshly parsed DSL. The
// clause tree is only materialized by Build, which the caller may not have
// run yet, so the DSL is built on a clone — the caller's instance is left
// untouched.
//
// Rules are matched through the instance's naming strategy (see validateAs):
// the parser has already converted field names, so a rule registered as
// "userEmail" must fire for the parsed field "user_email" — exactly the dual
// matching FieldsPlugin applies to its ignore list and whitelist.
func (p *ValidationPlugin) AfterParse(f figo.Figo, _ string) error {
	c := f.Clone()
	c.Build(nil)

	fn := f.GetNamingFunc() // never nil: SnakeCaseNaming is the default

	var vErr error
	c.Walk(func(n figo.Expr) {
		if vErr != nil {
			return
		}
		field, values, ok := exprFieldValues(n)
		if !ok {
			return
		}
		for _, v := range values {
			if err := p.validateAs(fn, field, v); err != nil {
				vErr = err
				return
			}
		}
	})
	return vErr
}

// AddRule adds a validation rule
func (p *ValidationPlugin) AddRule(rule ValidationRule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

// RegisterValidator registers a custom validator
func (p *ValidationPlugin) RegisterValidator(validator Validator) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.validators[validator.GetRuleName()] = validator
}

// Validate validates a field value against all applicable rules, matching
// rule fields verbatim (or as "*"). The AfterParse hook goes through
// validateAs instead, which additionally matches naming-converted spellings.
func (p *ValidationPlugin) Validate(field string, value any) error {
	return p.validateAs(nil, field, value)
}

// validateAs runs every rule whose Field matches: verbatim, as "*", or (when
// fn is non-nil) in its naming-converted form — so the same registration
// spelling works for ValidationPlugin and FieldsPlugin alike.
//
// A matching rule with neither a Handler nor a registered validator for its
// Rule name FAILS the validation: silently skipping it (the old behavior)
// meant a typo in the rule name switched the check off entirely.
func (p *ValidationPlugin) validateAs(fn figo.NamingFunc, field string, value any) error {
	// Snapshot rules and validators under the lock, then run the user's
	// handlers WITHOUT it — a handler calling AddRule/RegisterValidator
	// must not deadlock.
	p.mu.RLock()
	rules := make([]ValidationRule, len(p.rules))
	copy(rules, p.rules)
	validators := make(map[string]Validator, len(p.validators))
	for k, v := range p.validators {
		validators[k] = v
	}
	p.mu.RUnlock()

	for _, rule := range rules {
		match := rule.Field == field || rule.Field == "*"
		if !match && fn != nil {
			match = fn(rule.Field) == field
		}
		if !match {
			continue
		}

		if rule.Handler != nil {
			if err := rule.Handler(field, rule.Rule, value); err != nil {
				return validationError(field, rule.Message, err)
			}
			continue
		}
		validator, exists := validators[rule.Rule]
		if !exists {
			return fmt.Errorf("validation rule %q for field %s has no registered validator and no handler", rule.Rule, rule.Field)
		}
		if err := validator.Validate(field, rule.Rule, value); err != nil {
			return validationError(field, rule.Message, err)
		}
	}
	return nil
}

// validationError wraps a rule violation. With no configured Message the
// validator's own error is surfaced instead of an empty suffix.
func validationError(field, message string, err error) error {
	if message == "" {
		return fmt.Errorf("validation failed for field %s: %v", field, err)
	}
	return fmt.Errorf("validation failed for field %s: %s", field, message)
}

// exprFieldValues returns the field a leaf node filters on together with the
// values to validate. It reads the pointer nodes Walk hands its visitor.
// Nodes with no user-supplied value to check (logical nodes, IsNull/NotNull,
// figo.OrderBy, GeoDistance coordinates) report false.
func exprFieldValues(e figo.Expr) (string, []any, bool) {
	switch v := e.(type) {
	case *figo.EqExpr:
		return v.Field, []any{v.Value}, true
	case *figo.NeqExpr:
		return v.Field, []any{v.Value}, true
	case *figo.GtExpr:
		return v.Field, []any{v.Value}, true
	case *figo.GteExpr:
		return v.Field, []any{v.Value}, true
	case *figo.LtExpr:
		return v.Field, []any{v.Value}, true
	case *figo.LteExpr:
		return v.Field, []any{v.Value}, true
	case *figo.LikeExpr:
		return v.Field, []any{v.Value}, true
	case *figo.ILikeExpr:
		return v.Field, []any{v.Value}, true
	case *figo.RegexExpr:
		return v.Field, []any{v.Value}, true
	case *figo.InExpr:
		return v.Field, v.Values, true
	case *figo.NotInExpr:
		return v.Field, v.Values, true
	case *figo.BetweenExpr:
		return v.Field, []any{v.Low, v.High}, true
	case *figo.JsonPathExpr:
		return v.Field, []any{v.Value}, true
	case *figo.ArrayContainsExpr:
		return v.Field, v.Values, true
	case *figo.ArrayOverlapsExpr:
		return v.Field, v.Values, true
	case *figo.FullTextSearchExpr:
		return v.Field, []any{v.Query}, true
	case *figo.CustomExpr:
		return v.Field, []any{v.Value}, true
	default:
		return "", nil, false
	}
}

// Built-in validators
type RequiredValidator struct{}

func (v RequiredValidator) Validate(field, rule string, value any) error {
	if value == nil || value == "" {
		return fmt.Errorf("field %s is required", field)
	}
	return nil
}
func (v RequiredValidator) GetRuleName() string { return "required" }

type MinLengthValidator struct{}

func (v MinLengthValidator) Validate(field, rule string, value any) error {
	if str, ok := value.(string); ok {
		if len(str) < 3 { // Example minimum length
			return fmt.Errorf("field %s must be at least 3 characters", field)
		}
	}
	return nil
}
func (v MinLengthValidator) GetRuleName() string { return "min_length" }

type EmailValidator struct{}

func (v EmailValidator) Validate(field, rule string, value any) error {
	if str, ok := value.(string); ok {
		if !strings.Contains(str, "@") {
			return fmt.Errorf("field %s must be a valid email", field)
		}
	}
	return nil
}
func (v EmailValidator) GetRuleName() string { return "email" }
