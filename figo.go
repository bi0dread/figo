package figo

import (
	"github.com/gobeam/stringy"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strconv"
	"strings"
)

type Operation string

const (
	OperationEq      Operation = "eq"
	OperationGt      Operation = "gt"
	OperationGte     Operation = "gte"
	OperationLt      Operation = "lt"
	OperationLte     Operation = "lte"
	OperationNeq     Operation = "ne"
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
)

var currentParentFilter *filter

type Page struct {
	Skip int
	Take int
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

type filter struct {
	Type       int
	Operation  Operation
	Expression []clause.Expression
	Values     any
	Field      string
	Children   []*filter
	Paretn     *filter
}

func New() Figo {
	f := &figo{filters: make([]filter, 0), page: Page{
		Skip: 0,
		Take: 20,
	}, preloads: make([]any, 0), mainFilter: &filter{}, banFields: map[string]bool{}}

	currentParentFilter = &filter{Paretn: nil}
	f.mainFilter = currentParentFilter

	return f
}

type Figo interface {
	AddFiltersFromString(input string)
	AddFilter(opt Operation, exp clause.Expression)
	AddBanFields(fields ...string)
	GetBanFields() map[string]bool
	GetMainFilter() *filter
	GetClauses() []clause.Expression
	GetPreloads() []any
	GetPage() Page
	Apply(trx *gorm.DB) *gorm.DB
	Build()
}

type figo struct {
	filters    []filter
	clauses    []clause.Expression
	mainFilter *filter
	preloads   []any
	page       Page
	banFields  map[string]bool
}

func (f *figo) AddBanFields(fields ...string) {

	for _, field := range fields {
		f.banFields[field] = true
	}

}

func (f *figo) Build() {

	f.makeClauses()
}

func (f *figo) Apply(trx *gorm.DB) *gorm.DB {

	trx = trx.Limit(f.GetPage().Take)
	trx = trx.Offset(f.GetPage().Skip)

	// TODO uncomment
	/*	for _, preload := range f.preloads {
		trx = trx.Preload((preload).(string))
	}*/

	trx = trx.Clauses(f.GetClauses()...)

	return trx
}

func (f *figo) GetPage() Page {

	return f.page
}

func (f *figo) GetPreloads() []any {

	return f.preloads
}

func (f *figo) GetBanFields() map[string]bool {

	return f.banFields
}

func (f *figo) GetMainFilter() *filter {

	return f.mainFilter
}

func (f *figo) GetClauses() []clause.Expression {

	return f.clauses
}

func (f *figo) AddFiltersFromString(input string) {
	sectionSplit := strings.Split(input, "|")
	for _, section := range sectionSplit {
		f.operatorParser(section)

	}

}

func toArray(f clause.Expression) []clause.Expression {
	return []clause.Expression{f}
}

func (f *figo) AddFilter(opt Operation, exp clause.Expression) {
	fx := filter{}
	fx.Expression = append(fx.Expression, exp)
	fx.Operation = opt
	fx.Paretn = f.mainFilter

	switch v := exp.(type) {
	case clause.Eq:
		fx.Field = v.Column.(clause.Column).Name
		break
	case clause.Gt:
		fx.Field = v.Column.(clause.Column).Name
		break
	case clause.Gte:
		fx.Field = v.Column.(clause.Column).Name
		break
	case clause.Like:
		fx.Field = v.Column.(clause.Column).Name
		break
	case clause.Lte:
		fx.Field = v.Column.(clause.Column).Name
		break
	case clause.Lt:
		fx.Field = v.Column.(clause.Column).Name
		break
	case clause.Neq:
		fx.Field = v.Column.(clause.Column).Name
		break
	}

	f.mainFilter.Children = append(f.mainFilter.Children, &fx)
}

func (f *figo) operatorParser(str string) {

	if strings.Contains(str, ":") {
		fieldSplit := strings.SplitN(str, ":", 2)
		field := fieldSplit[0]
		field = stringy.New(field).SnakeCase("?", "").ToLower()
		fieldValue := fieldSplit[1]

		if fieldValue[:1] == "[" {
			fieldValue = fieldValue[1 : len(fieldValue)-1]
		}

		if field == string(OperationSort) {

			v := f.makeArrayFromString(fieldValue)

			var c []clause.OrderByColumn

			for _, a := range v {

				split := strings.Split(a.(string), "=")

				c = append(c, clause.OrderByColumn{
					Column: clause.Column{
						Name:  stringy.New(split[0]).SnakeCase("?", "").ToLower(),
						Table: clause.CurrentTable,
					},
					Desc:    strings.ToLower(split[1]) == "desc",
					Reorder: false,
				})

			}

			fx := filter{
				Type:      0,
				Operation: OperationSort,
				Expression: toArray(clause.OrderBy{
					Columns: c,
				}),
				Values: fieldValue,
				Field:  field,
				Paretn: currentParentFilter,
			}

			currentParentFilter.Children = append(currentParentFilter.Children, &fx)

		} else if field == string(OperationLoad) {
			f.preloads = append(f.preloads, f.makeArrayFromString(fieldValue)...)
		} else if field == string(OperationPage) {
			v := f.makeArrayFromString(fieldValue)

			for _, a := range v {
				split := strings.Split(a.(string), "=")

				item := split[0]
				value := split[1]

				parseInt, parsErr := strconv.ParseInt(value, 10, 64)
				if parsErr == nil {

					if item == "skip" {
						f.page.Skip = int(parseInt)
					} else if item == "take" {
						f.page.Take = int(parseInt)
					}

					f.page.validate()
				}

			}

		} else {
			actionSplit := strings.Split(fieldValue, ",")
			for _, action := range actionSplit {
				if strings.Contains(action, ":") {
					operatorSplit := strings.Split(action, ":")
					operator := operatorSplit[0]
					operatorValue := operatorSplit[1]

					if operator == string(OperationGt) {
						fx := filter{
							Type:      0,
							Operation: OperationGt,
							Expression: toArray(clause.Gt{
								Column: field,
								Value:  operatorValue,
							}),
							Values: operatorValue,
							Field:  field,
							Paretn: currentParentFilter,
						}

						currentParentFilter.Children = append(currentParentFilter.Children, &fx)

					} else if operator == string(OperationLt) {
						fx := filter{
							Type:      0,
							Operation: OperationLt,
							Expression: toArray(clause.Lt{
								Column: field,
								Value:  operatorValue,
							}),
							Values: operatorValue,
							Field:  field,
							Paretn: currentParentFilter,
						}

						currentParentFilter.Children = append(currentParentFilter.Children, &fx)

					} else if operator == string(OperationIn) {
						fx := filter{
							Type:      0,
							Operation: OperationIn,
							Expression: toArray(clause.IN{
								Column: field,
								Values: f.makeArrayFromString(operatorValue),
							}),
							Values: operatorValue,
							Field:  field,
							Paretn: currentParentFilter,
						}

						currentParentFilter.Children = append(currentParentFilter.Children, &fx)

					} else if operator == string(OperationNotIn) {
						fx := filter{
							Type:      0,
							Operation: OperationNotIn,
							Expression: toArray(clause.Not(clause.IN{
								Column: field,
								Values: f.makeArrayFromString(operatorValue),
							})),
							Values: operatorValue,
							Field:  field,
							Paretn: currentParentFilter,
						}

						currentParentFilter.Children = append(currentParentFilter.Children, &fx)

					} else if operator == string(OperationEq) {
						fx := filter{
							Type:      0,
							Operation: OperationEq,
							Expression: toArray(clause.Eq{
								Column: field,
								Value:  operatorValue,
							}),
							Values: operatorValue,
							Field:  field,
							Paretn: currentParentFilter,
						}

						currentParentFilter.Children = append(currentParentFilter.Children, &fx)

					} else if operator == string(OperationGte) {
						fx := filter{
							Type:      0,
							Operation: OperationGte,
							Expression: toArray(clause.Gte{
								Column: field,
								Value:  operatorValue,
							}),
							Values: operatorValue,
							Field:  field,
							Paretn: currentParentFilter,
						}

						currentParentFilter.Children = append(currentParentFilter.Children, &fx)

					} else if operator == string(OperationLte) {
						fx := filter{
							Type:      0,
							Operation: OperationLte,
							Expression: toArray(clause.Lte{
								Column: field,
								Value:  operatorValue,
							}),
							Values: operatorValue,
							Field:  field,
							Paretn: currentParentFilter,
						}

						currentParentFilter.Children = append(currentParentFilter.Children, &fx)

					} else if operator == string(OperationNeq) {
						fx := filter{
							Type:      0,
							Operation: OperationNeq,
							Expression: toArray(clause.Neq{
								Column: field,
								Value:  operatorValue,
							}),
							Values: operatorValue,
							Field:  field,
							Paretn: currentParentFilter,
						}

						currentParentFilter.Children = append(currentParentFilter.Children, &fx)

					} else if operator == string(OperationLike) {
						fx := filter{
							Type:      0,
							Operation: OperationLike,
							Expression: toArray(clause.Like{
								Column: field,
								Value:  operatorValue,
							}),
							Values: "%" + operatorValue + "%",
							Field:  field,
							Paretn: currentParentFilter,
						}

						currentParentFilter.Children = append(currentParentFilter.Children, &fx)

					} else if operator == string(OperationNotLike) {
						fx := filter{
							Type:      0,
							Operation: OperationNotLike,
							Expression: toArray(clause.Not(clause.Like{
								Column: field,
								Value:  "%" + operatorValue + "%",
							})),
							Values: operatorValue,
							Field:  field,
							Paretn: currentParentFilter,
						}

						currentParentFilter.Children = append(currentParentFilter.Children, &fx)

					} else if operator == string(OperationBetween) {
						v := f.makeArrayFromString(operatorValue)
						if len(v) >= 2 {
							fxGte := filter{
								Type:      0,
								Operation: OperationGte,
								Expression: toArray(clause.Gte{
									Column: field,
									Value:  v[0],
								}),
								Values: v[0],
								Field:  field,
								Paretn: currentParentFilter,
							}

							fxLte := filter{
								Type:      0,
								Operation: OperationLte,
								Expression: toArray(clause.Lte{
									Column: field,
									Value:  v[1],
								}),
								Values: v[1],
								Field:  field,
								Paretn: currentParentFilter,
							}

							currentParentFilter.Children = append(currentParentFilter.Children, &fxGte)
							currentParentFilter.Children = append(currentParentFilter.Children, &fxLte)
						}

					}
				} else {

					if currentParentFilter.Operation == OperationOr || currentParentFilter.Operation == OperationAnd || currentParentFilter.Operation == OperationNot {
						currentParentFilter = f.mainFilter
					}

					if action == string(OperationAnd) {

						gg := &filter{Operation: OperationAnd}
						gg.Paretn = currentParentFilter
						currentParentFilter.Children = append(currentParentFilter.Children, gg)
						currentParentFilter = gg

					} else if action == string(OperationOr) {
						gg := &filter{Operation: OperationOr}
						gg.Paretn = currentParentFilter
						currentParentFilter.Children = append(currentParentFilter.Children, gg)
						currentParentFilter = gg
					} else if action == string(OperationNot) {
						gg := &filter{Operation: OperationNot}
						gg.Paretn = currentParentFilter
						currentParentFilter.Children = append(currentParentFilter.Children, gg)
						currentParentFilter = gg
					}
				}

			}

		}

	} else {
		currentParentFilter = f.mainFilter

		if str == string(OperationOr) {

			fx := &filter{Operation: OperationOr}
			fx.Paretn = currentParentFilter
			currentParentFilter.Children = append(currentParentFilter.Children, fx)
			currentParentFilter = fx
		} else if str == string(OperationAnd) {
			fx := &filter{Operation: OperationAnd}
			fx.Paretn = currentParentFilter
			currentParentFilter.Children = append(currentParentFilter.Children, fx)
			currentParentFilter = fx
		} else if str == string(OperationNot) {
			fx := &filter{Operation: OperationNot}
			fx.Paretn = currentParentFilter
			currentParentFilter.Children = append(currentParentFilter.Children, fx)
			currentParentFilter = fx
		}

	}
}

func (f *figo) makeClauses() {

	f.recursiveItem(f.mainFilter)

	f.clauses = append(f.clauses, f.mainFilter.Expression...)

}

func (f *figo) recursiveItem(x *filter) {

	if len(x.Children) != 0 {

		for _, child := range x.Children {

			if _, ok := f.banFields[child.Field]; ok {
				continue
			}

			f.recursiveItem(child)
		}

		if x.Paretn != nil {
			if _, ok := f.banFields[x.Field]; !ok {
				x.Paretn.Expression = append(x.Paretn.Expression, x.Expression...)
			}
		}

	} else {
		if x.Paretn != nil {

			if _, ok := f.banFields[x.Field]; !ok {
				if x.Paretn.Operation == OperationOr {

					x.Paretn.Expression = append(x.Paretn.Expression, clause.Or(x.Expression...))

				} else if x.Paretn.Operation == OperationAnd {
					x.Paretn.Expression = append(x.Paretn.Expression, clause.And(x.Expression...))

				} else if x.Paretn.Operation == OperationNot {
					x.Paretn.Expression = append(x.Paretn.Expression, clause.Not(x.Expression...))

				} else {
					x.Paretn.Expression = append(x.Paretn.Expression, x.Expression...)
				}
			}

		}
	}

}

func (f *figo) makeArrayFromString(str string) []any {

	var result []any

	trimmedInput := strings.Trim(str, "[]")
	elements := strings.Split(trimmedInput, "&")

	for _, element := range elements {
		element = strings.TrimSpace(element)
		result = append(result, element)
	}

	return result
}
