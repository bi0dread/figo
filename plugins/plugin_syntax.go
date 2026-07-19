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
// rejects malformed input outright. With repair=true it first attempts to fix
// common malformations (trailing/leading and/or, unmatched parentheses,
// quotes, and brackets) and passes the repaired DSL on; input that still
// fails validation after repair is rejected. Repair means querying something
// other than what the caller literally sent — enable it deliberately.
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
func (p *SyntaxPlugin) AfterQuery(figo.Figo, any, interface{}) error { return nil }

// AfterParse implements Plugin
func (p *SyntaxPlugin) AfterParse(figo.Figo, string) error { return nil }

// BeforeParse validates the DSL (optionally repairing it first) and returns
// the input that should be parsed. Validation failures surface as *figo.ParseError
// with line/column positions.
func (p *SyntaxPlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) {
	if !p.repair {
		if err := validateDSLSyntax(dsl); err != nil {
			return "", err
		}
		return dsl, nil
	}

	fixed, err := attemptInputRepair(dsl)
	if err != nil {
		// Nothing repairable (or the repair didn't validate): accept the
		// original only if it is already valid.
		if validationErr := validateDSLSyntax(dsl); validationErr != nil {
			return "", validationErr
		}
		return dsl, nil
	}
	return fixed, nil
}

// validateDSLSyntax validates a DSL string with enhanced error reporting
func validateDSLSyntax(input string) error {
	// Validate parentheses with position tracking
	if err := validateParenthesesWithPosition(input); err != nil {
		return err
	}

	// Validate quotes with position tracking
	if err := validateQuotesWithPosition(input); err != nil {
		return err
	}

	// Validate brackets for load expressions
	if err := validateBrackets(input); err != nil {
		return err
	}

	// Validate basic syntax patterns
	if err := validateBasicSyntax(input); err != nil {
		return err
	}

	return nil
}

// attemptInputRepair tries to fix common malformed input patterns
func attemptInputRepair(input string) (string, error) {
	original := input
	fixed := input

	// Fix common patterns - be more conservative
	repairs := []struct {
		pattern     *regexp.Regexp
		replacement string
		description string
	}{
		{regexp.MustCompile(`\s+and\s*$`), "", "Remove trailing AND"},
		{regexp.MustCompile(`\s+or\s*$`), "", "Remove trailing OR"},
		{regexp.MustCompile(`\s+not\s*$`), "", "Remove trailing NOT"},
		{regexp.MustCompile(`^\s*and\b`), "", "Remove leading AND"},
		{regexp.MustCompile(`^\s*or\b`), "", "Remove leading OR"},
		// A leading NOT is valid and must never be stripped (filter inversion).
	}

	// Apply repairs
	for _, repair := range repairs {
		if repair.pattern.MatchString(fixed) {
			fixed = repair.pattern.ReplaceAllString(fixed, repair.replacement)
		}
	}

	// Try to fix unmatched parentheses
	if !validateParentheses(fixed) {
		fixed = fixUnmatchedParentheses(fixed)
	}

	// Try to fix unmatched quotes
	if !validateQuotes(fixed) {
		fixed = fixUnmatchedQuotes(fixed)
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

// validateParentheses checks if parentheses are properly matched
func validateParentheses(expr string) bool {
	count := 0
	for _, char := range expr {
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

// validateParenthesesWithPosition checks parentheses with detailed error reporting
func validateParenthesesWithPosition(expr string) error {
	count := 0
	line := 1
	column := 1
	var lastOpenPos int

	for i, char := range expr {
		if char == '\n' {
			line++
			column = 1
		} else {
			column++
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

// validateBrackets checks if brackets are properly matched for load expressions
func validateBrackets(expr string) error {
	count := 0
	line := 1
	column := 1
	var lastOpenPos int

	for i, char := range expr {
		if char == '\n' {
			line++
			column = 1
		} else {
			column++
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

// validateBasicSyntax checks for common syntax errors
func validateBasicSyntax(expr string) error {
	// Check for common malformed patterns
	patterns := []struct {
		pattern    string
		message    string
		suggestion string
	}{
		{`\s+and\s*$`, "incomplete AND expression", "Add field and value after AND"},
		{`\s+or\s*$`, "incomplete OR expression", "Add field and value after OR"},
		{`\s+not\s*$`, "incomplete NOT expression", "Add expression after NOT"},
		{`=\s*$`, "incomplete equality expression", "Add value after ="},
		{`>\s*$`, "incomplete greater than expression", "Add value after >"},
		{`<\s*$`, "incomplete less than expression", "Add value after <"},
		{`!=\s*$`, "incomplete not equal expression", "Add value after !="},
		{`>=\s*$`, "incomplete greater than or equal expression", "Add value after >="},
		{`<=\s*$`, "incomplete less than or equal expression", "Add value after <="},
		// The '^' must be escaped: unescaped it is a start-of-text anchor, so
		// `=^\s*$` could never match and a trailing LIKE operator slipped
		// through validation.
		{`=\^\s*$`, "incomplete LIKE expression", "Add value after =^"},
		{`!=\^\s*$`, "incomplete NOT LIKE expression", "Add value after !=^"},
		{`=~\s*$`, "incomplete regex expression", "Add value after =~"},
		{`!=~\s*$`, "incomplete NOT regex expression", "Add value after !=~"},
		{`<in>\s*$`, "incomplete IN expression", "Add value list after <in>"},
		{`<nin>\s*$`, "incomplete NOT IN expression", "Add value list after <nin>"},
		{`<bet>\s*$`, "incomplete BETWEEN expression", "Add value range after <bet>"},
		{`^\s*and\b`, "expression starts with AND", "Remove AND or add field before it"},
		{`^\s*or\b`, "expression starts with OR", "Remove OR or add field before it"},
		// A leading NOT is valid syntax ("not deleted=true") and must not be
		// flagged or "repaired" away — stripping it would invert the filter.
	}

	for _, p := range patterns {
		if matched, _ := regexp.MatchString(p.pattern, expr); matched {
			return &figo.ParseError{
				Message:    p.message,
				Position:   0,
				Line:       1,
				Column:     1,
				Context:    expr,
				Suggestion: p.suggestion,
			}
		}
	}

	return nil
}

// fixUnmatchedParentheses attempts to fix unmatched parentheses
func fixUnmatchedParentheses(input string) string {
	count := 0
	result := strings.Builder{}

	for _, char := range input {
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

// fixUnmatchedBrackets attempts to fix unmatched brackets
func fixUnmatchedBrackets(input string) string {
	count := 0
	result := strings.Builder{}

	for _, char := range input {
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
