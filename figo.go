package figo

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gobeam/stringy"
)

// Global configuration
var (
	// regexSQLOperator controls the SQL operator used for RegexExpr in SQL adapters.
	// Defaults to MySQL-compatible REGEXP. For Postgres, set to "~" or "~*".
	regexSQLOperator = "REGEXP"
)

// SetRegexSQLOperator sets the SQL operator used to render regex in SQL adapters (Raw/GORM)
func SetRegexSQLOperator(op string) {
	op = strings.TrimSpace(op)
	if op == "" {
		return
	}
	regexSQLOperator = op
}

// GetRegexSQLOperator returns the configured SQL regex operator
func GetRegexSQLOperator() string { return regexSQLOperator }

type NamingStrategy string

const NAMING_STRATEGY_NO_CHANGE = "no_change"
const NAMING_STRATEGY_SNAKE_CASE = "snake_case"

type Operation string

const (
	OperationEq       Operation = "="
	OperationGt       Operation = ">"
	OperationGte      Operation = ">="
	OperationLt       Operation = "<"
	OperationLte      Operation = "<="
	OperationNeq      Operation = "!="
	OperationNot      Operation = "not"
	OperationLike     Operation = "=^"
	OperationNotLike  Operation = "!=^"
	OperationRegex    Operation = "=~"
	OperationNotRegex Operation = "!=~"
	OperationAnd      Operation = "and"
	OperationOr       Operation = "or"
	OperationBetween  Operation = "<bet>"
	OperationIn       Operation = "<in>"
	OperationNotIn    Operation = "<nin>"
	OperationSort     Operation = "sort"
	OperationLoad     Operation = "load"
	OperationPage     Operation = "page"
	OperationChild    Operation = "----"
	OperationILike    Operation = ".=^"
	OperationIsNull   Operation = "<null>"
	OperationNotNull  Operation = "<notnull>"
)

// AdapterType removed: adapters are selected via Adapter objects

type Page struct {
	Skip int
	Take int
}

// Expr represents an ORM-agnostic expression node
type Expr interface{ isExpr() }

// Comparison expressions
type EqExpr struct {
	Field string
	Value any
}
type GteExpr struct {
	Field string
	Value any
}
type GtExpr struct {
	Field string
	Value any
}
type LtExpr struct {
	Field string
	Value any
}
type LteExpr struct {
	Field string
	Value any
}
type NeqExpr struct {
	Field string
	Value any
}
type LikeExpr struct {
	Field string
	Value any
}

// Regex expression
type RegexExpr struct {
	Field string
	Value any
}

// Logical expressions
type AndExpr struct{ Operands []Expr }
type OrExpr struct{ Operands []Expr }
type NotExpr struct{ Operands []Expr }

// Sorting expressions
type OrderByColumn struct {
	Name string
	Desc bool
}

type OrderBy struct{ Columns []OrderByColumn }

// Query is a marker interface for adapter-agnostic rendered queries
// Concrete types are provided per adapter (e.g., SQLQuery, MongoFindQuery)
type Query interface{ isQuery() }

// SQLQuery represents a parametrized SQL statement
type SQLQuery struct {
	SQL  string
	Args []any
}

func (SQLQuery) isQuery() {}

func (EqExpr) isExpr()    {}
func (GteExpr) isExpr()   {}
func (GtExpr) isExpr()    {}
func (LtExpr) isExpr()    {}
func (LteExpr) isExpr()   {}
func (NeqExpr) isExpr()   {}
func (LikeExpr) isExpr()  {}
func (RegexExpr) isExpr() {}
func (AndExpr) isExpr()   {}
func (OrExpr) isExpr()    {}
func (NotExpr) isExpr()   {}
func (OrderBy) isExpr()   {}

type InExpr struct {
	Field  string
	Values []any
}

type NotInExpr struct {
	Field  string
	Values []any
}

type BetweenExpr struct {
	Field string
	Low   any
	High  any
}

type IsNullExpr struct{ Field string }

type NotNullExpr struct{ Field string }

type ILikeExpr struct {
	Field string
	Value any
}

func (InExpr) isExpr()      {}
func (NotInExpr) isExpr()   {}
func (BetweenExpr) isExpr() {}
func (IsNullExpr) isExpr()  {}
func (NotNullExpr) isExpr() {}
func (ILikeExpr) isExpr()   {}

type Figo interface {
	AddFiltersFromString(input string)
	AddFilter(exp Expr)
	AddIgnoreFields(fields ...string)
	AddSelectFields(fields ...string)
	SetNamingStrategy(strategy NamingStrategy)
	SetPage(skip, take int)
	SetPageString(v string)
	SetAdapterObject(adapter Adapter)
	GetNamingStrategy() NamingStrategy
	GetIgnoreFields() map[string]bool
	GetSelectFields() map[string]bool
	GetClauses() []Expr
	GetPreloads() map[string][]Expr
	GetPage() Page
	GetAdapterObject() Adapter
	GetSqlString(ctx any, conditionType ...string) string
	GetExplainedSqlString(ctx any, conditionType ...string) string
	GetQuery(ctx any, conditionType ...string) Query
	Build()
}

type Adapter interface {
	GetSqlString(f Figo, ctx any, conditionType ...string) (string, bool)
	GetQuery(f Figo, ctx any, conditionType ...string) (Query, bool)
}

type figo struct {
	clauses        []Expr
	preloads       map[string][]Expr
	page           Page
	sort           *OrderBy
	ignoreFields   map[string]bool
	selectFields   map[string]bool
	dsl            string
	namingStrategy NamingStrategy
	adapterObj     Adapter
}

// Constructor: use New(adapter) with an Adapter object (or nil)

// New constructs a new instance with the specified adapter object. Pass nil for no adapter.
func New(adapter Adapter) Figo {
	f := &figo{page: Page{
		Skip: 0,
		Take: 20,
	}, preloads: make(map[string][]Expr), ignoreFields: make(map[string]bool), selectFields: make(map[string]bool), clauses: make([]Expr, 0), namingStrategy: NAMING_STRATEGY_SNAKE_CASE}
	f.adapterObj = adapter
	return f
}

func (p *Page) validate() {
	if p.Skip < 0 {
		p.Skip = 0
	}
	if p.Take < 0 {
		p.Take = 0
	}
}

type Node struct {
	Expression []Expr
	Operator   Operation
	Value      string
	Field      string
	Children   []*Node
	Parent     *Node
}

func (f *figo) parseDSL(expr string) *Node {
	root := &Node{Value: "root", Expression: make([]Expr, 0)}
	stack := []*Node{root}
	current := root
outerLoop:
	for i := 0; i < len(expr); {
		switch expr[i] {
		case '(':
			newNode := &Node{Operator: "----", Parent: current}
			current.Children = append(current.Children, newNode)
			stack = append(stack, newNode)
			current = newNode
			i++
		case ')':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
				current = stack[len(stack)-1]
			}
			i++
		case ' ':
			i++
		default:
			j := i
			ff := -1
			for j < len(expr) {

				if expr[j] == '"' && ff == -1 {
					ff = 1
					j++
					continue
				}
				if expr[j] == '"' && ff == 1 {
					ff = 0
					j++
					break

				}

				if expr[j] != '"' && ff == 1 {
					j++
					continue
				}

				if expr[j] == ' ' && ff == -1 {
					break
				}

				if expr[j] == ' ' && ff == 0 {
					break
				}

				j++
			}
			token := strings.TrimSpace(expr[i:j])
			if token != "" {
				if strings.HasPrefix(token, string(OperationSort)) || strings.HasPrefix(token, string(OperationPage)) || strings.HasPrefix(token, string(OperationLoad)) {
					k := j - 1
					if strings.HasPrefix(token, string(OperationLoad)) {
						bracketCount := 1
						for k < len(expr) && bracketCount > 0 {

							switch expr[k] {
							case '[':
								bracketCount++
							case ']':
								bracketCount--
							}
							k++

						}
						//k++

						loadLabel := fmt.Sprintf("%v=[", string(OperationLoad))

						v := strings.TrimSpace(expr[i:k])
						labelIndex := strings.Index(v, loadLabel)
						if labelIndex == -1 {
							i = k
							continue
						}
						content := v[labelIndex+len(loadLabel) : len(v)-1]
						if content == "" {
							i = k
							continue
						}

						loadSplit := strings.Split(content, "|")
						for _, l := range loadSplit {
							colonIndex := strings.Index(l, ":")
							if colonIndex == -1 {
								continue
							}
							rawTable := l[:colonIndex]
							table := strings.TrimSpace(rawTable)
							loadContent := strings.TrimSpace(l[colonIndex+1:])

							loadRootNode := f.parseDSL(loadContent)
							expressionParser(loadRootNode)
							loadExpr := getFinalExpr(*loadRootNode)
							if loadExpr != nil {
								f.preloads[table] = append(f.preloads[table], loadExpr)
							}

						}
						i = k
						continue

					} else if strings.HasPrefix(token, string(OperationPage)) {

						pageLabel := fmt.Sprintf("%v=", string(OperationPage))
						content := token[strings.Index(token, pageLabel)+len(pageLabel):]

						pageContent := strings.Split(content, ",")

						for _, s := range pageContent {
							pageSplit := strings.Split(s, ":")
							if len(pageSplit) != 2 {
								continue
							}

							field := pageSplit[0]
							value := pageSplit[1]

							parseInt, parsErr := strconv.ParseInt(value, 10, 64)
							if parsErr == nil {

								switch field {
								case "skip":
									f.page.Skip = int(parseInt)
								case "take":
									f.page.Take = int(parseInt)
								}

								f.page.validate()
							}

						}

					} else if strings.HasPrefix(token, string(OperationSort)) {

						sortLabel := fmt.Sprintf("%v=", string(OperationSort))
						content := token[strings.Index(token, sortLabel)+len(sortLabel):]

						sortContent := strings.Split(content, ",")

						var c []OrderByColumn

						for _, s := range sortContent {
							sortSplit := strings.Split(s, ":")
							if len(sortSplit) != 2 {
								continue
							}

							field := sortSplit[0]
							value := sortSplit[1]

							c = append(c, OrderByColumn{
								Name: f.parsFieldsName(field),
								Desc: strings.ToLower(value) == "desc",
							})

						}

						sortExpr := OrderBy{
							Columns: c,
						}
						f.sort = &sortExpr

					} else {
						for k < len(expr) && expr[k] != ' ' && expr[k] != '(' && expr[k] != ')' {
							k++
						}
					}

					i = k
				} else {
					// Try to combine tokens for expressions like "field > value" or "field =^ value"
					// Only do this for very specific cases to avoid interfering with complex operators
					combinedToken := token
					// Only combine if the token looks like a simple field name (alphanumeric + underscores)
					// and doesn't contain any operators or special characters
					if isSimpleFieldName(token) {
						// This looks like a field name with underscores, try to combine with next tokens
						nextStart := j
						for nextStart < len(expr) && expr[nextStart] == ' ' {
							nextStart++
						}
						if nextStart < len(expr) {
							nextEnd := nextStart
							nextFF := -1
							for nextEnd < len(expr) {
								if expr[nextEnd] == '"' && nextFF == -1 {
									nextFF = 1
									nextEnd++
									continue
								}
								if expr[nextEnd] == '"' && nextFF == 1 {
									nextFF = 0
									nextEnd++
									break
								}
								if expr[nextEnd] != '"' && nextFF == 1 {
									nextEnd++
									continue
								}
								if expr[nextEnd] == ' ' && nextFF == -1 {
									break
								}
								if expr[nextEnd] == ' ' && nextFF == 0 {
									break
								}
								nextEnd++
							}
							nextToken := strings.TrimSpace(expr[nextStart:nextEnd])
							// Combine with both simple and complex operators
							if nextToken == ">" || nextToken == "<" || nextToken == "=" || nextToken == "!=" || nextToken == ">=" || nextToken == "<=" || nextToken == "=^" || nextToken == "!=^" || nextToken == ".=^" || nextToken == "=~" || nextToken == "!=~" || nextToken == "<in>" || nextToken == "<nin>" || nextToken == "<bet>" || nextToken == "<null>" || nextToken == "<notnull>" {
								combinedToken = token + " " + nextToken
								j = nextEnd

								// Try to get the value token as well
								if nextToken == "<bet>" || nextToken == "<in>" || nextToken == "<nin>" {
									valueStart := j
									for valueStart < len(expr) && expr[valueStart] == ' ' {
										valueStart++
									}
									if valueStart < len(expr) {
										valueEnd := valueStart
										valueFF := -1
										parenCount := 0
										for valueEnd < len(expr) {
											if expr[valueEnd] == '"' && valueFF == -1 {
												valueFF = 1
												valueEnd++
												continue
											}
											if expr[valueEnd] == '"' && valueFF == 1 {
												valueFF = 0
												valueEnd++
												break
											}
											if expr[valueEnd] != '"' && valueFF == 1 {
												valueEnd++
												continue
											}
											// Handle parentheses for BETWEEN operations
											if expr[valueEnd] == '(' && valueFF == -1 {
												parenCount++
											}
											if expr[valueEnd] == ')' && valueFF == -1 {
												parenCount--
												if parenCount == 0 {
													valueEnd++
													break
												}
											}
											// Stop at spaces, parentheses, or logical operators (but not inside parentheses)
											if (expr[valueEnd] == ' ' || expr[valueEnd] == ')' || expr[valueEnd] == '(') && valueFF == -1 && parenCount == 0 {
												break
											}
											if expr[valueEnd] == ' ' && valueFF == 0 && parenCount == 0 {
												break
											}
											valueEnd++
										}
										valueToken := strings.TrimSpace(expr[valueStart:valueEnd])
										if valueToken != "" && !strings.Contains(valueToken, "=") && !strings.Contains(valueToken, ">") && !strings.Contains(valueToken, "<") && !strings.Contains(valueToken, "!") && valueToken != "and" && valueToken != "or" && valueToken != "not" && !strings.Contains(valueToken, "page=") && !strings.Contains(valueToken, "sort=") && !strings.Contains(valueToken, "load=") {
											combinedToken = combinedToken + " " + valueToken
											j = valueEnd
										}
									}
								} else {
									// For simple operators, use simpler value extraction
									valueStart := j
									for valueStart < len(expr) && expr[valueStart] == ' ' {
										valueStart++
									}
									if valueStart < len(expr) {
										valueEnd := valueStart
										valueFF := -1
										for valueEnd < len(expr) {
											if expr[valueEnd] == '"' && valueFF == -1 {
												valueFF = 1
												valueEnd++
												continue
											}
											if expr[valueEnd] == '"' && valueFF == 1 {
												valueFF = 0
												valueEnd++
												break
											}
											if expr[valueEnd] != '"' && valueFF == 1 {
												valueEnd++
												continue
											}
											// Stop at spaces, parentheses, or logical operators
											if (expr[valueEnd] == ' ' || expr[valueEnd] == ')' || expr[valueEnd] == '(') && valueFF == -1 {
												break
											}
											if expr[valueEnd] == ' ' && valueFF == 0 {
												break
											}
											valueEnd++
										}
										valueToken := strings.TrimSpace(expr[valueStart:valueEnd])
										if valueToken != "" && !strings.Contains(valueToken, "=") && !strings.Contains(valueToken, ">") && !strings.Contains(valueToken, "<") && !strings.Contains(valueToken, "!") && valueToken != "and" && valueToken != "or" && valueToken != "not" && !strings.Contains(valueToken, "page=") && !strings.Contains(valueToken, "sort=") && !strings.Contains(valueToken, "load=") {
											combinedToken = combinedToken + " " + valueToken
											j = valueEnd
										}
									}
								}
							}
						}
					}

					operator, valueStr, field := parseToken(combinedToken)
					value := f.parsFieldsValue(valueStr)

					// Check if this is a logical operator
					valueStrForOp := fmt.Sprintf("%v", value)
					if operator == "" && Operation(valueStrForOp) != OperationAnd && Operation(valueStrForOp) != OperationOr && Operation(valueStrForOp) != OperationNot {
						i = j
						continue
					} else {

						for ignoreField := range f.ignoreFields {
							if field == ignoreField {
								i = j
								continue outerLoop
							}
						}

					}

					newNode := &Node{Operator: operator, Value: valueStrForOp, Field: f.parsFieldsName(field), Parent: current, Expression: make([]Expr, 0)}
					if Operation(valueStrForOp) == OperationAnd || Operation(valueStrForOp) == OperationOr || Operation(valueStrForOp) == OperationNot {
						newNode.Operator = Operation(valueStrForOp)
					} else {

						newNode.Expression = append(newNode.Expression, getClausesFromOperation(operator, f.parsFieldsName(field), value))
					}
					current.Children = append(current.Children, newNode)
					i = j
				}
			} else {
				i = j
			}
		}
	}

	return root
}

func (f *figo) parsFieldsName(str string) string {
	switch f.namingStrategy {
	case NAMING_STRATEGY_NO_CHANGE:
		return str
	case NAMING_STRATEGY_SNAKE_CASE:
		return stringy.New(str).SnakeCase("?", "").ToLower()
	default:
		return ""
	}
}

func (f *figo) parsFieldsValue(str string) any {
	s := strings.TrimSpace(str)

	// Handle quoted strings - remove quotes but keep as string
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1] // Remove quotes
	}

	// Parse boolean values (only for unquoted values)
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}

	// Parse numeric values (only for unquoted values)
	if s != "" {
		// Try to parse as integer
		if intVal, err := strconv.ParseInt(s, 10, 64); err == nil {
			return intVal
		}
		// Try to parse as float
		if floatVal, err := strconv.ParseFloat(s, 64); err == nil {
			return floatVal
		}
	}

	// Return as string
	return s
}

// isSimpleFieldName checks if a token looks like a simple field name
func isSimpleFieldName(token string) bool {
	// Must not be empty
	if token == "" {
		return false
	}

	// Must not contain operators or special characters
	if strings.Contains(token, "=") || strings.Contains(token, ">") || strings.Contains(token, "<") ||
		strings.Contains(token, "!") || strings.Contains(token, "^") || strings.Contains(token, "~") ||
		strings.Contains(token, "page=") || strings.Contains(token, "sort=") || strings.Contains(token, "load=") {
		return false
	}

	// Must not be logical operators
	if token == "and" || token == "or" || token == "not" {
		return false
	}

	// Must not be complex operators
	if token == "like" || token == "in" || token == "between" || token == "null" || token == "notnull" {
		return false
	}

	// Must contain only alphanumeric characters and underscores
	for _, char := range token {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_') {
			return false
		}
	}

	return true
}

func parseToken(token string) (Operation, string, string) {
	// Order matters: place custom multi-char markers first
	operators := []Operation{
		OperationNotRegex,
		OperationRegex,
		OperationNotLike,
		OperationILike,
		OperationNotIn,
		OperationIn,
		OperationBetween,
		OperationNotNull,
		OperationIsNull,
		OperationLike,
		OperationGte, OperationLte,
		OperationNeq, OperationGt, OperationLt, OperationEq,
	}
	for _, op := range operators {
		if strings.Contains(token, string(op)) {
			parts := strings.Split(token, string(op))
			var right string
			if len(parts) > 1 {
				right = parts[1]
			}
			field := strings.TrimSpace(parts[0])
			return op, right, field
		}
	}
	return "", token, ""
}

func getClausesFromOperation(o Operation, field string, value any) Expr {
	// helper to parse a single scalar literal: preserve quoted strings, parse unquoted numerics
	parseScalarValue := func(raw string) any {
		s := strings.TrimSpace(raw)
		if len(s) >= 2 && strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
			return strings.Trim(s, "\"")
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
		if f64, err := strconv.ParseFloat(s, 64); err == nil {
			return f64
		}
		return s
	}

	// helper to parse a list literal like [1,2,"x"]
	parseListValue := func(raw string) []any {
		s := strings.TrimSpace(raw)
		if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
			s = strings.TrimPrefix(s, "[")
			s = strings.TrimSuffix(s, "]")
		}
		if s == "" {
			return nil
		}
		parts := strings.Split(s, ",")
		vals := make([]any, 0, len(parts))
		for _, p := range parts {
			vals = append(vals, parseScalarValue(p))
		}
		return vals
	}

	// helper to parse a string literal for LIKE operations (always string)
	parseLikeValue := func(raw string) string {
		s := strings.TrimSpace(raw)
		if len(s) >= 2 && strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
			return strings.Trim(s, "\"")
		}
		return s
	}

	switch o {
	case OperationEq:
		return EqExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationGte:
		return GteExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationGt:
		return GtExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationLt:
		return LtExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationLte:
		return LteExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationNeq:
		return NeqExpr{Field: field, Value: parseScalarValue(fmt.Sprintf("%v", value))}
	case OperationLike:
		return LikeExpr{Field: field, Value: parseLikeValue(fmt.Sprintf("%v", value))}
	case OperationNotLike:
		return NotExpr{Operands: []Expr{LikeExpr{Field: field, Value: parseLikeValue(fmt.Sprintf("%v", value))}}}
	case OperationRegex:
		return RegexExpr{Field: field, Value: parseLikeValue(fmt.Sprintf("%v", value))}
	case OperationNotRegex:
		return NotExpr{Operands: []Expr{RegexExpr{Field: field, Value: parseLikeValue(fmt.Sprintf("%v", value))}}}
	case OperationIn:
		vals := parseListValue(fmt.Sprintf("%v", value))
		return InExpr{Field: field, Values: vals}
	case OperationNotIn:
		vals := parseListValue(fmt.Sprintf("%v", value))
		return NotInExpr{Field: field, Values: vals}
	case OperationBetween:
		s := strings.TrimSpace(fmt.Sprintf("%v", value))
		// strip optional parentheses
		if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
			s = strings.TrimPrefix(s, "(")
			s = strings.TrimSuffix(s, ")")
		}
		if idx := strings.Index(s, ".."); idx > 0 {
			low := strings.TrimSpace(s[:idx])
			high := strings.TrimSpace(s[idx+2:])
			return BetweenExpr{Field: field, Low: parseScalarValue(low), High: parseScalarValue(high)}
		}
		return nil
	case OperationILike:
		return ILikeExpr{Field: field, Value: parseLikeValue(fmt.Sprintf("%v", value))}
	case OperationIsNull:
		return IsNullExpr{Field: field}
	case OperationNotNull:
		return NotNullExpr{Field: field}
	default:
		return nil
	}
}

func expressionParser(node *Node) {

	if node.Operator == OperationChild {

		if len(node.Children) == 1 {
			node.Expression = append(node.Expression, node.Children[0].Expression...)
		}

		var latestExpr Expr

		for i, child := range node.Children {
			if child.Operator == OperationAnd {

				if latestExpr == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExpr = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []Expr
				v = append(v, latestExpr)

				if i+1 < len(node.Children) {
					if node.Children[i+1].Operator == OperationChild {
						expressionParser(node.Children[i+1])
					}

					if len(node.Children[i+1].Expression) == 0 {
						continue
					}
					v = append(v, node.Children[i+1].Expression[len(node.Children[i+1].Expression)-1])
				} else {
					continue
				}

				exp := AndExpr{Operands: v}
				latestExpr = exp

				child.Expression = append(child.Expression, exp)

				node.Expression = append(node.Expression, child.Expression...)

			} else if child.Operator == OperationOr {
				if latestExpr == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExpr = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []Expr
				v = append(v, latestExpr)

				if i+1 < len(node.Children) {
					if node.Children[i+1].Operator == OperationChild {
						expressionParser(node.Children[i+1])
					}

					v = append(v, node.Children[i+1].Expression[len(node.Children[i+1].Expression)-1])
				} else {
					continue
				}

				exp := OrExpr{Operands: v}
				latestExpr = exp

				child.Expression = append(child.Expression, exp)

				node.Expression = append(node.Expression, child.Expression...)
			} else if child.Operator == OperationNot {
				// NOT operation should only take one operand
				if latestExpr == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExpr = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []Expr
				v = append(v, latestExpr)

				exp := NotExpr{Operands: v}
				latestExpr = exp

				child.Expression = append(child.Expression, exp)

				node.Expression = append(node.Expression, child.Expression...)

			} else if child.Operator == OperationChild {
				expressionParser(child)
			}
		}
	} else {
		var latestExpr Expr
		for i, child := range node.Children {

			if child.Operator == OperationAnd {

				if latestExpr == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExpr = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []Expr
				v = append(v, latestExpr)

				if i+1 < len(node.Children) {
					if node.Children[i+1].Operator == OperationChild {
						expressionParser(node.Children[i+1])
					}

					if len(node.Children[i+1].Expression) == 0 {
						continue
					}
					v = append(v, node.Children[i+1].Expression[len(node.Children[i+1].Expression)-1])
				} else {
					continue
				}

				exp := AndExpr{Operands: v}
				latestExpr = exp

				child.Expression = append(child.Expression, exp)

			} else if child.Operator == OperationOr {
				if latestExpr == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExpr = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []Expr
				v = append(v, latestExpr)

				if i+1 < len(node.Children) {
					if node.Children[i+1].Operator == OperationChild {
						expressionParser(node.Children[i+1])
					}

					v = append(v, node.Children[i+1].Expression[len(node.Children[i+1].Expression)-1])
				} else {
					continue
				}

				exp := OrExpr{Operands: v}
				latestExpr = exp

				child.Expression = append(child.Expression, exp)
			} else if child.Operator == OperationNot {
				// NOT operation should only take one operand
				if latestExpr == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExpr = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []Expr
				v = append(v, latestExpr)

				exp := NotExpr{Operands: v}
				latestExpr = exp

				child.Expression = append(child.Expression, exp)
			} else if child.Operator == OperationChild {
				expressionParser(child)
			}
		}
	}

}

func getFinalExpr(node Node) Expr {

	if len(node.Children) == 0 {
		return nil
	} else if len(node.Children) == 1 {

		if len(node.Children[0].Expression) == 1 {
			return node.Children[0].Expression[0]
		}
		for i := len(node.Children[0].Expression) - 1; i >= 0; i-- {

			if node.Children[0].Operator == OperationAnd || node.Children[0].Operator == OperationOr || node.Children[0].Operator == OperationNot || node.Children[0].Operator == OperationChild {

				return node.Children[0].Expression[i]

			}
		}

	} else {
		for i := len(node.Children) - 1; i >= 0; i-- {
			if node.Children[i].Operator == OperationAnd || node.Children[i].Operator == OperationOr || node.Children[i].Operator == OperationNot || node.Children[i].Operator == OperationChild {
				if node.Children[i].Operator == OperationChild {
					continue
				}
				if len(node.Children[i].Expression) == 0 {
					continue
				}
				return node.Children[i].Expression[len(node.Children[i].Expression)-1]
			}
		}
	}

	return nil

}

func (f *figo) AddFiltersFromString(input string) {

	f.dsl = input
}

func (f *figo) AddFilter(exp Expr) {
	f.clauses = append(f.clauses, exp)
}

func (f *figo) AddIgnoreFields(fields ...string) {

	for _, field := range fields {
		f.ignoreFields[field] = true
	}
}

func (f *figo) AddSelectFields(fields ...string) {

	for _, field := range fields {
		f.selectFields[field] = true
	}
}

func (f *figo) GetIgnoreFields() map[string]bool {

	return f.ignoreFields
}

func (f *figo) GetClauses() []Expr {

	return f.clauses
}

func (f *figo) GetPreloads() map[string][]Expr {

	return f.preloads
}

func (f *figo) GetPage() Page {

	return f.page
}

func (f *figo) SetPage(skip, take int) {

	f.page.Skip = skip
	f.page.Take = take
	f.page.validate()
}

func (f *figo) SetPageString(v string) {
	pageContent := strings.Split(v, ",")

	for _, s := range pageContent {
		pageSplit := strings.Split(s, ":")
		if len(pageSplit) != 2 {
			continue
		}

		field := pageSplit[0]
		value := pageSplit[1]

		parseInt, parsErr := strconv.ParseInt(value, 10, 64)
		if parsErr == nil {
			switch field {
			case "skip":
				f.page.Skip = int(parseInt)
			case "take":
				f.page.Take = int(parseInt)
			}

			f.page.validate()
		}

	}
}

func (f *figo) GetSelectFields() map[string]bool {

	return f.selectFields
}

func (f *figo) SetNamingStrategy(strategy NamingStrategy) {
	f.namingStrategy = strategy
}

func (f *figo) GetNamingStrategy() NamingStrategy {

	return f.namingStrategy
}

func (f *figo) SetAdapterObject(adapter Adapter) {
	f.adapterObj = adapter
}

func (f *figo) GetAdapterObject() Adapter {
	return f.adapterObj
}

// GetSqlString returns a SQL string based on the selected adapter.
// For AdapterGorm, ctx should be a *gorm.DB configured with Model(...).
// For AdapterRaw, ctx can be a table name (string) or RawContext.
func (f *figo) GetSqlString(ctx any, conditionType ...string) string {
	if f.adapterObj != nil {
		if sql, ok := f.adapterObj.GetSqlString(f, ctx, conditionType...); ok {
			return sql
		}
		return ""
	}
	return ""
}

// GetExplainedSqlString returns a SQL string with placeholders expanded for easier debugging
func (f *figo) GetExplainedSqlString(ctx any, conditionType ...string) string {
	if f.adapterObj != nil {
		if sql, ok := f.adapterObj.GetSqlString(f, ctx, conditionType...); ok {
			return sql
		}
		return ""
	}
	return ""
}

// GetQuery delegates to the configured adapter to obtain a typed query object
func (f *figo) GetQuery(ctx any, conditionType ...string) Query {
	if f.adapterObj != nil {
		if q, ok := f.adapterObj.GetQuery(f, ctx, conditionType...); ok {
			return q
		}
		return nil
	}
	return nil
}

func (f *figo) Build() {
	if f.dsl == "" {
		return
	}
	root := f.parseDSL(f.dsl)
	expressionParser(root)

	finalExpr := getFinalExpr(*root)

	if finalExpr != nil {
		f.clauses = append(f.clauses, finalExpr)

	}

}
