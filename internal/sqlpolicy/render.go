package sqlpolicy

import (
	"sort"
	"strconv"
	"strings"
)

func renderExecutable(agentSQL string, referenced []string, products map[string]ProductGrant, rowLimit int64) (string, error) {
	var builder strings.Builder
	if len(referenced) != 0 {
		builder.WriteString("WITH ")
		for index, name := range referenced {
			product, ok := products[name]
			if !ok {
				return "", reject(CodeInvalidGrant)
			}
			if index != 0 {
				builder.WriteString(",\n")
			}
			builder.WriteString(quoteIdentifier(product.LogicalName))
			builder.WriteString(" AS (\n  SELECT ")
			for columnIndex, column := range product.ApprovedColumns {
				if columnIndex != 0 {
					builder.WriteString(", ")
				}
				builder.WriteString(quoteIdentifier(column))
			}
			builder.WriteString("\n  FROM ")
			builder.WriteString(quoteIdentifier(product.PhysicalSchema))
			builder.WriteByte('.')
			builder.WriteString(quoteIdentifier(product.PhysicalView))
			if len(product.MandatoryScope) != 0 {
				builder.WriteString("\n  WHERE ")
				predicates := append([]ScopePredicate(nil), product.MandatoryScope...)
				sort.SliceStable(predicates, func(i, j int) bool {
					if predicates[i].Column == predicates[j].Column {
						return predicates[i].Operator < predicates[j].Operator
					}
					return predicates[i].Column < predicates[j].Column
				})
				for predicateIndex, predicate := range predicates {
					if predicateIndex != 0 {
						builder.WriteString(" AND ")
					}
					renderPredicate(&builder, predicate)
				}
			}
			builder.WriteString("\n)")
		}
		builder.WriteByte('\n')
	}
	builder.WriteString("SELECT *\nFROM (\n")
	builder.WriteString(agentSQL)
	builder.WriteString("\n) AS \"__taskbound_result\"\nLIMIT ")
	builder.WriteString(strconv.FormatInt(rowLimit, 10))
	return builder.String(), nil
}

func renderPredicate(builder *strings.Builder, predicate ScopePredicate) {
	builder.WriteString(quoteIdentifier(predicate.Column))
	switch predicate.Operator {
	case ScopeEqual:
		builder.WriteString(" = ")
		builder.WriteString(quoteLiteral(predicate.Values[0]))
	case ScopeNotEqual:
		builder.WriteString(" <> ")
		builder.WriteString(quoteLiteral(predicate.Values[0]))
	case ScopeLess:
		builder.WriteString(" < ")
		builder.WriteString(quoteLiteral(predicate.Values[0]))
	case ScopeLessEqual:
		builder.WriteString(" <= ")
		builder.WriteString(quoteLiteral(predicate.Values[0]))
	case ScopeGreater:
		builder.WriteString(" > ")
		builder.WriteString(quoteLiteral(predicate.Values[0]))
	case ScopeGreaterEqual:
		builder.WriteString(" >= ")
		builder.WriteString(quoteLiteral(predicate.Values[0]))
	case ScopeIn, ScopeNotIn:
		if predicate.Operator == ScopeNotIn {
			builder.WriteString(" NOT IN (")
		} else {
			builder.WriteString(" IN (")
		}
		for index, value := range predicate.Values {
			if index != 0 {
				builder.WriteString(", ")
			}
			builder.WriteString(quoteLiteral(value))
		}
		builder.WriteByte(')')
	case ScopeIsNull:
		builder.WriteString(" IS NULL")
	case ScopeIsNotNull:
		builder.WriteString(" IS NOT NULL")
	}
}

func validPredicateShape(predicate ScopePredicate) bool {
	switch predicate.Operator {
	case ScopeEqual, ScopeNotEqual, ScopeLess, ScopeLessEqual, ScopeGreater, ScopeGreaterEqual:
		return len(predicate.Values) == 1
	case ScopeIn, ScopeNotIn:
		return len(predicate.Values) > 0
	case ScopeIsNull, ScopeIsNotNull:
		return len(predicate.Values) == 0
	default:
		return false
	}
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quoteLiteral(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `'`, `''`)
	return `E'` + value + `'`
}
