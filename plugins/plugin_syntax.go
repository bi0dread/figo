package plugins

import (
	figo "github.com/bi0dread/figo/v4"
)

import (
	"fmt"
	"regexp"
	"strings"
)

// DSL syntax validation (and optional auto-repair) is provided as a plugin.
// AddFiltersFromString itself accepts input as-is; register a SyntaxPlugin to
// reject malformed DSL with a structured *figo.ParseError — or, with repair
// enabled, to fix common malformations before parsing.
//
//	f.RegisterPlugin(plugins.NewSyntaxPlugin(false)) // strict: reject malformed DSL
//	f.RegisterPlugin(plugins.NewSyntaxPlugin(true))  // repair what can be fixed, then validate
//	err := f.AddFiltersFromString(`(name="john" and age>25`) // repaired or *figo.ParseError
type SyntaxPlugin struct {
	repair bool
}

// NewSyntaxPlugin creates a syntax plugin. With repair=false, BeforeParse
// rejects malformed input outright: unbalanced parentheses/quotes/brackets,
// dangling connectors and operators, and doubled equality operators
// (`name == 5`, `name = = 5`). With repair=true it first attempts to fix
// common malformations (trailing/leading and/or, unmatched parentheses,
// quotes, and brackets) and passes the repaired DSL on; input that still
// fails validation after repair is rejected. Repair means querying something
// other than what the caller literally sent — enable it deliberately.
//
// All structural checks and fixers are quote-aware: characters inside a
// double-quoted value are literal to the core parser (name="a)b" is valid
// DSL), so they neither count toward balance checks nor get "repaired" away.
//
// Input the core parser demonstrably accepts as-is is never rejected or
// repaired, even when a heuristic check flags it (see BeforeParse): `x<null>`,
// a base64 value's trailing '=', name=O'Brien, email=~x==y all pass through
// unchanged in both modes.
func NewSyntaxPlugin(repair bool) *SyntaxPlugin {
	return &SyntaxPlugin{repair: repair}
}

// Name implements Plugin
func (p *SyntaxPlugin) Name() string { return "figo-syntax" }

// Version implements Plugin
func (p *SyntaxPlugin) Version() string { return "1.0.0" }

// Initialize implements Plugin
func (p *SyntaxPlugin) Initialize(figo.Figo) error { return nil }

// BeforeQuery implements Plugin
func (p *SyntaxPlugin) BeforeQuery(figo.Figo, any) error { return nil }

// AfterQuery implements Plugin
func (p *SyntaxPlugin) AfterQuery(figo.Figo, any, any) error { return nil }

// AfterParse implements Plugin
func (p *SyntaxPlugin) AfterParse(figo.Figo, string) error { return nil }

// BeforeParse validates the DSL (optionally repairing it first) and returns
// the input that should be parsed. Validation failures surface as *figo.ParseError
// with line/column positions.
//
// Several validators over-approximate the core parser's grammar: a base64
// value's trailing '=', `<null>`/`<notnull>` ending in '>', a possessive
// apostrophe (name=O'Brien), '==' inside an unquoted regex value. When such a
// gate-eligible check fails but the core parser demonstrably accepts the
// input as-is (parserAcceptsCleanly), the DSL passes through UNCHANGED — in
// strict and repair mode alike; repair used to "fix" valid input into a query
// for different data (or into one whose WHERE rendered empty).
func (p *SyntaxPlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) {
	vErr, gateable := validateDSLSyntaxClassified(dsl)
	if vErr == nil {
		return dsl, nil
	}
	if gateable && parserAcceptsCleanly(dsl) {
		return dsl, nil
	}

	if !p.repair {
		return "", vErr
	}

	fixed, err := attemptInputRepair(dsl)
	if err != nil {
		// Nothing repairable (or the repair didn't validate): reject with
		// the original validation error.
		return "", vErr
	}
	return fixed, nil
}

// syntaxProbeAdapter is the no-op figo.Adapter handed to the scratch BuildE
// inside the parse-validity gate — the gate only parses, never renders.
type syntaxProbeAdapter struct{}

func (syntaxProbeAdapter) GetSqlString(figo.Figo, any, ...string) (string, bool) { return "", true }
func (syntaxProbeAdapter) GetQuery(figo.Figo, any, ...string) (figo.Query, bool) { return nil, true }

// parserAcceptsCleanly reports whether the core parser accepts dsl exactly as
// written: a scratch instance (no plugins) parses it with zero diagnostics,
// produces at least one clause/preload/sort, and the parsed tree carries no
// tell-tale of a swallowed malformation. The tree inspection matters because
// the parser is permissive rather than rejecting: `a==b` "parses" with zero
// diagnostics into Eq(a, "=b"), `name="x` into a value containing the raw
// quote, and `a >=` into an empty value — none of which may pass the gate.
func parserAcceptsCleanly(dsl string) bool {
	scratch := figo.New()
	if err := scratch.AddFiltersFromString(dsl); err != nil {
		return false
	}
	if err := scratch.BuildE(syntaxProbeAdapter{}); err != nil {
		return false
	}
	clauses := scratch.GetClauses()
	preloads := scratch.GetPreloads()
	if len(clauses) == 0 && len(preloads) == 0 && scratch.GetSort() == nil {
		return false // the input was dropped wholesale, not parsed
	}
	for _, e := range clauses {
		if !parsedExprLooksClean(e) {
			return false
		}
	}
	for _, exprs := range preloads {
		for _, e := range exprs {
			if !parsedExprLooksClean(e) {
				return false
			}
		}
	}
	return true
}

// parsedExprLooksClean vets one parsed expression for the artifacts the
// permissive parser leaves behind when it swallows genuinely-broken DSL: an
// empty field, an empty string value (`a >=` → Gte(a, "")), a raw '"' inside
// a field or value (an unclosed quote folded into the value), or an
// equality value starting with '=' (a doubled operator: `a==b` → Eq(a, "=b")).
// Types this package doesn't model (advanced/adapter-specific) pass — the
// parser only emits them from well-formed constructs.
//
// Known over-rejection: a DSL combining a QUOTED value that legitimately
// starts with '=' or an intentionally empty quoted value with a separate
// validator false positive (e.g. `a="=b" and x<null>`) fails the gate and
// falls back to the validation error — the status-quo rejection, never a
// wrong query.
func parsedExprLooksClean(e figo.Expr) bool {
	switch x := e.(type) {
	case figo.EqExpr:
		return cleanParsedField(x.Field) && cleanParsedValue(x.Value) && !doubledEqualsValue(x.Value)
	case figo.NeqExpr:
		return cleanParsedField(x.Field) && cleanParsedValue(x.Value) && !doubledEqualsValue(x.Value)
	case figo.GtExpr:
		return cleanParsedField(x.Field) && cleanParsedValue(x.Value)
	case figo.GteExpr:
		return cleanParsedField(x.Field) && cleanParsedValue(x.Value)
	case figo.LtExpr:
		return cleanParsedField(x.Field) && cleanParsedValue(x.Value)
	case figo.LteExpr:
		return cleanParsedField(x.Field) && cleanParsedValue(x.Value)
	case figo.LikeExpr:
		return cleanParsedField(x.Field) && cleanParsedValue(x.Value)
	case figo.ILikeExpr:
		return cleanParsedField(x.Field) && cleanParsedValue(x.Value)
	case figo.RegexExpr:
		return cleanParsedField(x.Field) && cleanParsedValue(x.Value)
	case figo.InExpr:
		return cleanParsedField(x.Field) && cleanParsedValues(x.Values)
	case figo.NotInExpr:
		return cleanParsedField(x.Field) && cleanParsedValues(x.Values)
	case figo.BetweenExpr:
		return cleanParsedField(x.Field) && cleanParsedValue(x.Low) && cleanParsedValue(x.High)
	case figo.IsNullExpr:
		return cleanParsedField(x.Field)
	case figo.NotNullExpr:
		return cleanParsedField(x.Field)
	case figo.AndExpr:
		return allParsedExprsLookClean(x.Operands)
	case figo.OrExpr:
		return allParsedExprsLookClean(x.Operands)
	case figo.NotExpr:
		return allParsedExprsLookClean(x.Operands)
	default:
		return true
	}
}

func allParsedExprsLookClean(exprs []figo.Expr) bool {
	for _, e := range exprs {
		if !parsedExprLooksClean(e) {
			return false
		}
	}
	return true
}

// cleanParsedField reports whether a parsed field name is plausible: non-empty
// and free of raw quote characters.
func cleanParsedField(field string) bool {
	return field != "" && !strings.ContainsRune(field, '"')
}

// cleanParsedValue accepts any non-string value; string values must be
// non-empty (an empty one is a trailing operator's missing operand) and free
// of raw '"' (a properly quoted value has its quotes stripped by the parser,
// so a surviving quote means the quoting was broken).
func cleanParsedValue(v any) bool {
	s, ok := v.(string)
	if !ok {
		return true
	}
	return s != "" && !strings.ContainsRune(s, '"')
}

func cleanParsedValues(vs []any) bool {
	for _, v := range vs {
		if !cleanParsedValue(v) {
			return false
		}
	}
	return true
}

// doubledEqualsValue reports the parse artifact of a doubled equality
// operator: the parser folds the second '=' into the value (`a==b` becomes
// Eq(a, "=b")), so an unquoted equality value starting with '=' marks input
// that must stay rejected.
func doubledEqualsValue(v any) bool {
	s, ok := v.(string)
	return ok && strings.HasPrefix(s, "=")
}

// validateDSLSyntax validates a DSL string with enhanced error reporting
func validateDSLSyntax(input string) error {
	err, _ := validateDSLSyntaxClassified(input)
	return err
}

// validateDSLSyntaxClassified reports the first validation failure plus
// whether that failure class is gate-ELIGIBLE. Quote, doubled-equality and
// trailing-operator checks over-approximate the parser's grammar and may be
// overridden by the parse-validity gate when the core parser accepts the
// input as-is. Paren/bracket imbalance and dangling/leading connectors are
// never gated: the parser silently ignores stray parens and drops dangling
// connectors, so "parses cleanly" proves nothing for those classes.
func validateDSLSyntaxClassified(input string) (error, bool) {
	// Validate parentheses with position tracking
	if err := validateParenthesesWithPosition(input); err != nil {
		return err, false
	}

	// Validate quotes with position tracking
	if err := validateQuotesWithPosition(input); err != nil {
		return err, true
	}

	// Validate brackets for load expressions
	if err := validateBrackets(input); err != nil {
		return err, false
	}

	// Validate basic syntax patterns
	return validateBasicSyntaxClassified(input)
}

// syntaxRepairs are the conservative pattern rewrites repair mode applies,
// compiled once. A leading NOT is valid and must never be stripped (filter
// inversion).
var syntaxRepairs = []struct {
	pattern     *regexp.Regexp
	replacement string
	description string
}{
	{regexp.MustCompile(`\s+and\s*$`), "", "Remove trailing AND"},
	{regexp.MustCompile(`\s+or\s*$`), "", "Remove trailing OR"},
	{regexp.MustCompile(`\s+not\s*$`), "", "Remove trailing NOT"},
	{regexp.MustCompile(`^\s*and\b`), "", "Remove leading AND"},
	{regexp.MustCompile(`^\s*or\b`), "", "Remove leading OR"},
}

// attemptInputRepair tries to fix common malformed input patterns
func attemptInputRepair(input string) (string, error) {
	original := input
	fixed := input

	// Fix quotes FIRST — before the pattern rewrites and the quote-aware
	// paren/bracket fixers. Closing a dangling quote at the END of the input
	// (never editing inside it) means (a) the trailing-token rewrites below
	// can no longer delete text that is actually part of an unclosed quoted
	// value (`name="foo and` must become `name="foo and"`, not `name="foo"`),
	// and (b) the paren/bracket fixers see the value's true extent and never
	// edit inside it (fixing parens first appended the missing ')' inside
	// the region the closing quote was about to capture).
	if !validateQuotes(fixed) {
		fixed = fixUnmatchedQuotes(fixed)
	}

	// Apply repairs. These run on quote-balanced input, so a trailing
	// connector inside a quoted value is shielded by the closing quote and
	// only genuine dangling connectors are stripped.
	for _, repair := range syntaxRepairs {
		if repair.pattern.MatchString(fixed) {
			fixed = repair.pattern.ReplaceAllString(fixed, repair.replacement)
		}
	}

	// Try to fix unmatched parentheses
	if !validateParentheses(fixed) {
		fixed = fixUnmatchedParentheses(fixed)
	}

	// Try to fix unmatched brackets
	if err := validateBrackets(fixed); err != nil {
		fixed = fixUnmatchedBrackets(fixed)
	}

	// If no changes were made, return original
	if fixed == original {
		return original, fmt.Errorf("no repairs could be applied")
	}

	// Validate the fixed input
	if err := validateDSLSyntax(fixed); err != nil {
		return original, fmt.Errorf("repair failed validation: %w", err)
	}

	return fixed, nil
}

// validateParentheses checks if parentheses are properly matched. Parens
// inside a double-quoted value are literal to the core parser (name="a)b" is
// valid DSL) and never count.
func validateParentheses(expr string) bool {
	count := 0
	inQuote := false
	for _, char := range expr {
		if char == '"' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		if char == '(' {
			count++
		} else if char == ')' {
			count--
			if count < 0 {
				return false // Unmatched closing parenthesis
			}
		}
	}
	return count == 0 // All parentheses matched
}

// validateParenthesesWithPosition checks parentheses with detailed error
// reporting, skipping double-quoted regions (see validateParentheses).
func validateParenthesesWithPosition(expr string) error {
	count := 0
	line := 1
	column := 1
	inQuote := false
	var lastOpenPos int

	for i, char := range expr {
		if char == '\n' {
			line++
			column = 1
		} else {
			column++
		}

		if char == '"' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}

		if char == '(' {
			count++
			lastOpenPos = i
		} else if char == ')' {
			count--
			if count < 0 {
				return &figo.ParseError{
					Message:    "unmatched closing parenthesis",
					Position:   i,
					Line:       line,
					Column:     column,
					Context:    expr,
					Suggestion: "Remove extra closing parenthesis or add opening parenthesis",
				}
			}
		}
	}

	if count > 0 {
		return &figo.ParseError{
			Message:    "unmatched opening parenthesis",
			Position:   lastOpenPos,
			Line:       line,
			Column:     column,
			Context:    expr,
			Suggestion: "Add closing parenthesis to match opening one",
		}
	}

	return nil
}

// validateQuotes checks if quotes are properly matched
func validateQuotes(expr string) bool {
	inQuotes := false
	quoteChar := rune(0)

	for _, char := range expr {
		if char == '"' || char == '\'' {
			if !inQuotes {
				inQuotes = true
				quoteChar = char
			} else if char == quoteChar {
				inQuotes = false
				quoteChar = 0
			}
		}
	}

	return !inQuotes // All quotes properly closed
}

// validateQuotesWithPosition checks quotes with detailed error reporting
func validateQuotesWithPosition(expr string) error {
	inQuotes := false
	quoteChar := rune(0)
	line := 1
	column := 1
	var quoteStartPos int

	for i, char := range expr {
		if char == '\n' {
			line++
			column = 1
		} else {
			column++
		}

		if char == '"' || char == '\'' {
			if !inQuotes {
				inQuotes = true
				quoteChar = char
				quoteStartPos = i
			} else if char == quoteChar {
				inQuotes = false
				quoteChar = 0
			}
		}
	}

	if inQuotes {
		return &figo.ParseError{
			Message:    "unmatched quote",
			Position:   quoteStartPos,
			Line:       line,
			Column:     column,
			Context:    expr,
			Suggestion: "Add closing quote to match opening one",
		}
	}

	return nil
}

// validateBrackets checks if brackets are properly matched for load and list
// expressions, skipping double-quoted regions (name="a[b" is valid DSL).
func validateBrackets(expr string) error {
	count := 0
	line := 1
	column := 1
	inQuote := false
	var lastOpenPos int

	for i, char := range expr {
		if char == '\n' {
			line++
			column = 1
		} else {
			column++
		}

		if char == '"' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}

		if char == '[' {
			count++
			lastOpenPos = i
		} else if char == ']' {
			count--
			if count < 0 {
				return &figo.ParseError{
					Message:    "unmatched closing bracket",
					Position:   i,
					Line:       line,
					Column:     column,
					Context:    expr,
					Suggestion: "Remove extra closing bracket or add opening bracket",
				}
			}
		}
	}

	if count > 0 {
		return &figo.ParseError{
			Message:    "unmatched opening bracket",
			Position:   lastOpenPos,
			Line:       line,
			Column:     column,
			Context:    expr,
			Suggestion: "Add closing bracket to match opening one",
		}
	}

	return nil
}

// basicSyntaxChecks are the trailing/leading-token patterns strict validation
// applies, compiled once (they used to be recompiled on every call). ORDER
// MATTERS: multi-character operators come before their single-character
// prefixes/suffixes, so "a >=" reports the >=-specific message instead of
// matching the bare `=\s*$` pattern first.
//
// A leading NOT is valid syntax ("not deleted=true") and must not be flagged
// or "repaired" away — stripping it would invert the filter.
//
// gateable marks the checks that can false-positive on parser-valid DSL
// (`x<null>` ends in '>', a base64 value ends in '=') and may therefore be
// overridden by the parse-validity gate. The connector checks are grammar-true
// (a dangling `and` is silently DROPPED by the parser, never parsed) and stay
// authoritative.
var basicSyntaxChecks = []struct {
	pattern    *regexp.Regexp
	message    string
	suggestion string
	gateable   bool
}{
	{regexp.MustCompile(`\s+and\s*$`), "incomplete AND expression", "Add field and value after AND", false},
	{regexp.MustCompile(`\s+or\s*$`), "incomplete OR expression", "Add field and value after OR", false},
	{regexp.MustCompile(`\s+not\s*$`), "incomplete NOT expression", "Add expression after NOT", false},
	// The '^' must be escaped: unescaped it is a start-of-text anchor, so
	// `=^\s*$` could never match and a trailing LIKE operator slipped
	// through validation.
	{regexp.MustCompile(`!=\^\s*$`), "incomplete NOT LIKE expression", "Add value after !=^", true},
	{regexp.MustCompile(`=\^\s*$`), "incomplete LIKE expression", "Add value after =^", true},
	{regexp.MustCompile(`!=~\s*$`), "incomplete NOT regex expression", "Add value after !=~", true},
	{regexp.MustCompile(`=~\s*$`), "incomplete regex expression", "Add value after =~", true},
	{regexp.MustCompile(`>=\s*$`), "incomplete greater than or equal expression", "Add value after >=", true},
	{regexp.MustCompile(`<=\s*$`), "incomplete less than or equal expression", "Add value after <=", true},
	{regexp.MustCompile(`!=\s*$`), "incomplete not equal expression", "Add value after !=", true},
	{regexp.MustCompile(`<in>\s*$`), "incomplete IN expression", "Add value list after <in>", true},
	{regexp.MustCompile(`<nin>\s*$`), "incomplete NOT IN expression", "Add value list after <nin>", true},
	{regexp.MustCompile(`<bet>\s*$`), "incomplete BETWEEN expression", "Add value range after <bet>", true},
	{regexp.MustCompile(`=\s*$`), "incomplete equality expression", "Add value after =", true},
	{regexp.MustCompile(`>\s*$`), "incomplete greater than expression", "Add value after >", true},
	{regexp.MustCompile(`<\s*$`), "incomplete less than expression", "Add value after <", true},
	{regexp.MustCompile(`^\s*and\b`), "expression starts with AND", "Remove AND or add field before it", false},
	{regexp.MustCompile(`^\s*or\b`), "expression starts with OR", "Remove OR or add field before it", false},
}

// validateBasicSyntax checks for common syntax errors
func validateBasicSyntax(expr string) error {
	err, _ := validateBasicSyntaxClassified(expr)
	return err
}

// validateBasicSyntaxClassified is validateBasicSyntax plus the failing
// check's gate eligibility (see validateDSLSyntaxClassified).
func validateBasicSyntaxClassified(expr string) (error, bool) {
	if idx := doubledEqualsIndex(expr); idx >= 0 {
		// Gate-eligible: '==' inside an unquoted regex VALUE (email=~x==y)
		// is legal DSL the operator-position heuristic can still misread.
		return &figo.ParseError{
			Message:    "doubled equality operator",
			Position:   idx,
			Line:       1,
			Column:     idx + 1,
			Context:    expr,
			Suggestion: "Use a single '=' (quote the value if it starts with '=')",
		}, true
	}

	for _, p := range basicSyntaxChecks {
		if p.pattern.MatchString(expr) {
			return &figo.ParseError{
				Message:    p.message,
				Position:   0,
				Line:       1,
				Column:     1,
				Context:    expr,
				Suggestion: p.suggestion,
			}, p.gateable
		}
	}

	return nil, false
}

// doubledEqualsIndex reports the position of a doubled equality operator —
// an '=' in operator position whose next non-blank character is another '='
// (`name == 5`, `name = = 5`, `name===5`) — or -1. No DSL operator contains
// two consecutive '=', so these always parse as a predicate on an empty
// field name and used to slip through strict validation entirely.
//
// Quote-aware: '=' inside a double-quoted value is literal. To stay out of
// unquoted VALUE content (a regex like `email=~x==y`), only an '=' preceded
// by a field-name character or blank is considered operator position.
func doubledEqualsIndex(expr string) int {
	inQuote := false
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if c == '"' {
			inQuote = !inQuote
			continue
		}
		if inQuote || c != '=' {
			continue
		}
		if i > 0 && !isFieldOrBlankByte(expr[i-1]) {
			continue
		}
		j := i + 1
		for j < len(expr) && (expr[j] == ' ' || expr[j] == '\t') {
			j++
		}
		if j < len(expr) && expr[j] == '=' {
			return i
		}
	}
	return -1
}

// isFieldOrBlankByte reports whether b can end a field name (letters, digits,
// '_', '.', '-', '$', any non-ASCII byte) or is a blank — the byte kinds that
// can legitimately precede a comparison operator.
func isFieldOrBlankByte(b byte) bool {
	switch {
	case b == ' ' || b == '\t':
		return true
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_' || b == '.' || b == '-' || b == '$':
		return true
	case b >= 0x80: // continuation/start byte of a non-ASCII (Unicode) field name
		return true
	default:
		return false
	}
}

// fixUnmatchedParentheses attempts to fix unmatched parentheses. Parens
// inside a double-quoted value are part of the value — editing them queried a
// silently different string (name="a)b" became name="ab").
func fixUnmatchedParentheses(input string) string {
	count := 0
	inQuote := false
	result := strings.Builder{}

	for _, char := range input {
		if char == '"' {
			inQuote = !inQuote
			result.WriteRune(char)
			continue
		}
		if inQuote {
			result.WriteRune(char)
			continue
		}
		if char == '(' {
			count++
			result.WriteRune(char)
		} else if char == ')' {
			if count > 0 {
				count--
				result.WriteRune(char)
			}
			// Skip extra closing parentheses
		} else {
			result.WriteRune(char)
		}
	}

	// Add missing closing parentheses
	for i := 0; i < count; i++ {
		result.WriteRune(')')
	}

	return result.String()
}

// fixUnmatchedQuotes attempts to fix unmatched quotes
func fixUnmatchedQuotes(input string) string {
	inQuotes := false
	quoteChar := rune(0)
	result := strings.Builder{}

	for _, char := range input {
		if char == '"' || char == '\'' {
			if !inQuotes {
				inQuotes = true
				quoteChar = char
				result.WriteRune(char)
			} else if char == quoteChar {
				inQuotes = false
				quoteChar = 0
				result.WriteRune(char)
			} else {
				result.WriteRune(char)
			}
		} else {
			result.WriteRune(char)
		}
	}

	// Add missing closing quote
	if inQuotes {
		result.WriteRune(quoteChar)
	}

	return result.String()
}

// fixUnmatchedBrackets attempts to fix unmatched brackets, leaving brackets
// inside double-quoted values alone (they are literal value characters).
func fixUnmatchedBrackets(input string) string {
	count := 0
	inQuote := false
	result := strings.Builder{}

	for _, char := range input {
		if char == '"' {
			inQuote = !inQuote
			result.WriteRune(char)
			continue
		}
		if inQuote {
			result.WriteRune(char)
			continue
		}
		if char == '[' {
			count++
			result.WriteRune(char)
		} else if char == ']' {
			if count > 0 {
				count--
				result.WriteRune(char)
			}
			// Skip extra closing brackets
		} else {
			result.WriteRune(char)
		}
	}

	// Add missing closing brackets
	for i := 0; i < count; i++ {
		result.WriteRune(']')
	}

	return result.String()
}
