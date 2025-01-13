package figo

import (
	"fmt"
	"github.com/gobeam/stringy"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strconv"
	"strings"
)

type NamingStrategy string

const NAMING_STRATEGY_NO_CHANGE = "no_change"
const NAMING_STRATEGY_SNAKE_CASE = "snake_case"

type Operation string

const (
	OperationEq      Operation = "="
	OperationGt      Operation = ">"
	OperationGte     Operation = ">="
	OperationLt      Operation = "<"
	OperationLte     Operation = "<="
	OperationNeq     Operation = "!="
	OperationNot     Operation = "not"
	OperationLike    Operation = "like"
	OperationNotLike Operation = "notLike"
	OperationAnd     Operation = "and"
	OperationOr      Operation = "or"
	OperationBetween Operation = "between"
	OperationIn      Operation = "in"
	OperationNotIn   Operation = "notIn"
	OperationSort    Operation = "sort"
	OperationLoad    Operation = "load"
	OperationPage    Operation = "page"
	OperationChild   Operation = "----"
)

type Page struct {
	Skip int
	Take int
}

type Figo interface {
	AddFiltersFromString(input string)
	AddFilter(exp clause.Expression)
	AddIgnoreFields(fields ...string)
	AddSelectFields(fields ...string)
	SetNamingStrategy(strategy NamingStrategy)
	GetNamingStrategy() NamingStrategy
	GetIgnoreFields() map[string]bool
	GetSelectFields() map[string]bool
	GetClauses() []clause.Expression
	GetPreloads() map[string][]clause.Expression
	GetPage() Page
	Apply(trx *gorm.DB) *gorm.DB
	Build()
}

type figo struct {
	clauses        []clause.Expression
	preloads       map[string][]clause.Expression
	page           Page
	sort           clause.Expression
	ignoreFields   map[string]bool
	selectFields   map[string]bool
	dsl            string
	namingStrategy NamingStrategy
}

func New() Figo {
	f := &figo{page: Page{
		Skip: 0,
		Take: 20,
	}, preloads: make(map[string][]clause.Expression), ignoreFields: make(map[string]bool), selectFields: make(map[string]bool), clauses: make([]clause.Expression, 0), namingStrategy: NAMING_STRATEGY_SNAKE_CASE}

	return f
}

func (p *Page) validate() {
	if p.Skip < 0 {
		p.Skip = 0
	}
	if p.Take < 0 {
		p.Take = 0
	} else if p.Take > 20 {
		p.Take = 20
	}
}

type Node struct {
	Expression []clause.Expression
	Operator   Operation
	Value      string
	Field      string
	Children   []*Node
	Parent     *Node
}

func (f *figo) parseDSL(expr string) *Node {
	root := &Node{Value: "root", Expression: make([]clause.Expression, 0)}
	stack := []*Node{root}
	current := root

	for i := 0; i < len(expr); {
		switch expr[i] {
		case '(':
			newNode := &Node{Operator: "----", Parent: current}
			current.Children = append(current.Children, newNode)
			stack = append(stack, newNode)
			current = newNode
			i++
		case ')':
			stack = stack[:len(stack)-1]
			current = stack[len(stack)-1]
			i++
		case ' ':
			i++
		default:
			j := i
			ff := -1
			for j < len(expr) && expr[j] != '(' && expr[j] != ')' {

				if expr[j] == '"' && ff == -1 {
					ff = 1
					j++
					continue
				}

				if expr[j] == '"' && ff == 1 {
					ff = 0

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

							if expr[k] == '[' {
								bracketCount++
							} else if expr[k] == ']' {
								bracketCount--
							}
							k++

						}
						//k++

						loadLabel := fmt.Sprintf("%v=[", string(OperationLoad))

						v := strings.TrimSpace(expr[i:k])
						content := v[strings.Index(v, loadLabel)+len(loadLabel) : len(v)-1]
						if content == "" {
							i = k
							continue
						}

						loadSplit := strings.Split(content, "|")
						for _, l := range loadSplit {
							table := l[:strings.Index(l, ":")]
							loadContent := strings.TrimSpace(l[len(table)+1:])

							loadRootNode := f.parseDSL(loadContent)
							expressionParser(loadRootNode)
							loadExpr := getFinalExpr(*loadRootNode)
							f.preloads[table] = append(f.preloads[table], loadExpr)

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

								if field == "skip" {
									f.page.Skip = int(parseInt)
								} else if field == "take" {
									f.page.Take = int(parseInt)
								}

								f.page.validate()
							}

						}

					} else if strings.HasPrefix(token, string(OperationSort)) {

						sortLabel := fmt.Sprintf("%v=", string(OperationSort))
						content := token[strings.Index(token, sortLabel)+len(sortLabel):]

						sortContent := strings.Split(content, ",")

						var c []clause.OrderByColumn

						for _, s := range sortContent {
							sortSplit := strings.Split(s, ":")
							if len(sortSplit) != 2 {
								continue
							}

							field := sortSplit[0]
							value := sortSplit[1]

							c = append(c, clause.OrderByColumn{
								Column: clause.Column{
									Name:  f.parsFieldsName(field),
									Table: clause.CurrentTable,
								},
								Desc:    strings.ToLower(value) == "desc",
								Reorder: false,
							})

						}

						sortExpr := clause.OrderBy{
							Columns: c,
						}
						f.sort = sortExpr

					} else {
						for k < len(expr) && expr[k] != ' ' && expr[k] != '(' && expr[k] != ')' {
							k++
						}
					}

					i = k
				} else {
					operator, value, field := parseToken(token)
					value = f.parsFieldsValue(value)

					if operator == "" && Operation(value) != OperationAnd && Operation(value) != OperationOr && Operation(value) != OperationNot {
						i = j
						continue
					}

					newNode := &Node{Operator: operator, Value: value, Field: f.parsFieldsName(field), Parent: current, Expression: make([]clause.Expression, 0)}
					if Operation(value) == OperationAnd || Operation(value) == OperationOr || Operation(value) == OperationNot {
						newNode.Operator = Operation(value)
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
	if f.namingStrategy == NAMING_STRATEGY_NO_CHANGE {
		return str
	} else if f.namingStrategy == NAMING_STRATEGY_SNAKE_CASE {
		return stringy.New(str).SnakeCase("?", "").ToLower()

	}
	return ""
}

func (f *figo) parsFieldsValue(str string) string {
	return strings.Replace(str, "\"", "", -1)
}

func parseToken(token string) (Operation, string, string) {
	operators := []Operation{OperationGte, OperationLte, OperationNeq, OperationGt, OperationLt, OperationEq}
	for _, op := range operators {
		if strings.Contains(token, string(op)) {
			parts := strings.Split(token, string(op))
			return op, parts[1], parts[0]
		}
	}
	return "", token, ""
}

func getClausesFromOperation(o Operation, field string, value any) clause.Expression {
	switch o {
	case OperationEq:
		return clause.Eq{Column: field, Value: value}
	case OperationGte:
		return clause.Gte{Column: field, Value: value}
	case OperationGt:
		return clause.Gt{Column: field, Value: value}
	case OperationLt:
		return clause.Lt{Column: field, Value: value}
	case OperationLte:
		return clause.Lte{Column: field, Value: value}
	case OperationNeq:
		return clause.Neq{Column: field, Value: value}

	default:
		return nil
	}

}

func expressionParser(node *Node) {

	if node.Operator == OperationChild {

		if len(node.Children) == 1 {
			node.Expression = append(node.Expression, node.Children[0].Expression...)
		}

		var latestExp clause.Expression

		for i, child := range node.Children {
			if child.Operator == OperationAnd {

				if latestExp == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExp = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []clause.Expression
				v = append(v, latestExp)

				if node.Children[i+1].Operator == OperationChild {
					expressionParser(node.Children[i+1])
				}

				v = append(v, node.Children[i+1].Expression[len(node.Children[i+1].Expression)-1])

				exp := clause.And(v...)
				latestExp = exp

				child.Expression = append(child.Expression, exp)

				node.Expression = append(node.Expression, child.Expression...)

			} else if child.Operator == OperationOr {
				if latestExp == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExp = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []clause.Expression
				v = append(v, latestExp)

				if node.Children[i+1].Operator == OperationChild {
					expressionParser(node.Children[i+1])
				}

				v = append(v, node.Children[i+1].Expression[len(node.Children[i+1].Expression)-1])

				exp := clause.Or(v...)
				latestExp = exp

				child.Expression = append(child.Expression, exp)

				node.Expression = append(node.Expression, child.Expression...)
			} else if child.Operator == OperationNot {
				if latestExp == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExp = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []clause.Expression
				v = append(v, latestExp)

				if node.Children[i+1].Operator == OperationChild {
					expressionParser(node.Children[i+1])
				}

				v = append(v, node.Children[i+1].Expression[len(node.Children[i+1].Expression)-1])

				exp := clause.Not(v...)
				latestExp = exp

				child.Expression = append(child.Expression, exp)

				node.Expression = append(node.Expression, child.Expression...)

			} else if child.Operator == OperationChild {
				expressionParser(child)
			}
		}
	} else {
		var latestExp clause.Expression
		for i, child := range node.Children {

			if child.Operator == OperationAnd {

				if latestExp == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExp = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []clause.Expression
				v = append(v, latestExp)

				if node.Children[i+1].Operator == OperationChild {
					expressionParser(node.Children[i+1])
				}

				v = append(v, node.Children[i+1].Expression[len(node.Children[i+1].Expression)-1])

				exp := clause.And(v...)
				latestExp = exp

				child.Expression = append(child.Expression, exp)

			} else if child.Operator == OperationOr {
				if latestExp == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExp = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []clause.Expression
				v = append(v, latestExp)

				if node.Children[i+1].Operator == OperationChild {
					expressionParser(node.Children[i+1])
				}

				v = append(v, node.Children[i+1].Expression[len(node.Children[i+1].Expression)-1])

				exp := clause.Or(v...)
				latestExp = exp

				child.Expression = append(child.Expression, exp)
			} else if child.Operator == OperationNot {
				if latestExp == nil {
					if i > 0 && len(node.Children[i-1].Expression) > 0 {
						latestExp = node.Children[i-1].Expression[len(node.Children[i-1].Expression)-1]
					} else {
						continue
					}
				}

				var v []clause.Expression
				v = append(v, latestExp)
				v = append(v, node.Children[i+1].Expression[len(node.Children[i+1].Expression)-1])

				exp := clause.Not(v...)
				latestExp = exp

				child.Expression = append(child.Expression, exp)
			} else if child.Operator == OperationChild {
				expressionParser(child)
			}
		}
	}

}

func getFinalExpr(node Node) clause.Expression {

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
				return node.Children[i].Expression[len(node.Children[i].Expression)-1]
			}
		}
	}

	return nil

}

func (f *figo) AddFiltersFromString(input string) {

	f.dsl = input
}

func (f *figo) AddFilter(exp clause.Expression) {
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

func (f *figo) GetClauses() []clause.Expression {

	return f.clauses
}

func (f *figo) GetPreloads() map[string][]clause.Expression {

	return f.preloads
}

func (f *figo) GetPage() Page {

	return f.page
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

func (f *figo) Apply(trx *gorm.DB) *gorm.DB {
	trx = trx.Limit(f.GetPage().Take)
	trx = trx.Offset(f.GetPage().Skip)

	for k, v := range f.preloads {
		trx = trx.Preload(k, v)
	}

	trx = trx.Clauses(f.GetClauses()...)

	if f.sort != nil {
		trx = trx.Clauses(f.sort)
	}

	return trx
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
