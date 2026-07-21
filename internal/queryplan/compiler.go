package queryplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/internal/deepseek"
)

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

type Product struct {
	Name              string
	Columns           map[string]struct{}
	AllowedFunctions  map[string]struct{}
	AllowedAggregates map[string]struct{}
}

func Compile(plan deepseek.QueryPlan, product Product) (string, error) {
	if plan.Product != product.Name {
		return "", errors.New("query plan product is not approved")
	}
	if !safeIdentifier(product.Name) {
		return "", errors.New("invalid product identifier")
	}
	selects := make([]string, 0, len(plan.Columns)+len(plan.Aggregates))
	for _, column := range plan.Columns {
		if err := allowedColumn(column, product); err != nil {
			return "", err
		}
		selects = append(selects, quoteIdentifier(column))
	}
	for _, aggregate := range plan.Aggregates {
		fn := strings.ToLower(aggregate.Function)
		if _, ok := product.AllowedAggregates[fn]; !ok || !safeIdentifier(fn) {
			return "", fmt.Errorf("aggregate %q is not allowed", aggregate.Function)
		}
		if err := allowedColumn(aggregate.Column, product); err != nil {
			return "", err
		}
		if !safeIdentifier(aggregate.Alias) {
			return "", errors.New("aggregate alias is invalid")
		}
		selects = append(selects, fn+"("+quoteIdentifier(aggregate.Column)+") AS "+quoteIdentifier(aggregate.Alias))
	}
	if len(selects) == 0 {
		return "", errors.New("empty select list")
	}

	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(strings.Join(selects, ", "))
	b.WriteString(" FROM ")
	b.WriteString(quoteIdentifier(product.Name))
	if len(plan.Filters) > 0 {
		filters := make([]string, 0, len(plan.Filters))
		for _, filter := range plan.Filters {
			if err := allowedColumn(filter.Column, product); err != nil {
				return "", err
			}
			op := strings.ToUpper(strings.TrimSpace(filter.Op))
			switch op {
			case "=", "!=", "<>", "<", "<=", ">", ">=", "LIKE":
				literal, err := sqlLiteral(filter.Value)
				if err != nil {
					return "", err
				}
				filters = append(filters, quoteIdentifier(filter.Column)+" "+op+" "+literal)
			case "IN", "NOT IN":
				values, ok := filter.Value.([]any)
				if !ok || len(values) == 0 || len(values) > 100 {
					return "", errors.New("IN requires a non-empty JSON array of at most 100 values")
				}
				literals := make([]string, 0, len(values))
				for _, value := range values {
					literal, err := sqlLiteral(value)
					if err != nil {
						return "", err
					}
					literals = append(literals, literal)
				}
				filters = append(filters, quoteIdentifier(filter.Column)+" "+op+" ("+strings.Join(literals, ", ")+")")
			default:
				return "", fmt.Errorf("filter operator %q is not allowed", filter.Op)
			}
		}
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(filters, " AND "))
	}
	if len(plan.GroupBy) > 0 {
		groups := make([]string, 0, len(plan.GroupBy))
		for _, column := range plan.GroupBy {
			if err := allowedColumn(column, product); err != nil {
				return "", err
			}
			groups = append(groups, quoteIdentifier(column))
		}
		b.WriteString(" GROUP BY ")
		b.WriteString(strings.Join(groups, ", "))
	}
	if len(plan.OrderBy) > 0 {
		orders := make([]string, 0, len(plan.OrderBy))
		aliases := make(map[string]struct{}, len(plan.Aggregates))
		for _, aggregate := range plan.Aggregates {
			aliases[aggregate.Alias] = struct{}{}
		}
		for _, order := range plan.OrderBy {
			if _, ok := aliases[order.Column]; !ok {
				if err := allowedColumn(order.Column, product); err != nil {
					return "", err
				}
			}
			direction := strings.ToUpper(order.Direction)
			if direction == "" {
				direction = "ASC"
			}
			if direction != "ASC" && direction != "DESC" {
				return "", errors.New("invalid order direction")
			}
			orders = append(orders, quoteIdentifier(order.Column)+" "+direction)
		}
		b.WriteString(" ORDER BY ")
		b.WriteString(strings.Join(orders, ", "))
	}
	if plan.Limit > 0 {
		b.WriteString(" LIMIT ")
		b.WriteString(strconv.Itoa(plan.Limit))
	}
	return b.String(), nil
}

func CatalogPrompt(products []Product) (string, error) {
	type promptProduct struct {
		Name       string   `json:"name"`
		Columns    []string `json:"columns"`
		Aggregates []string `json:"aggregates"`
	}
	prompt := make([]promptProduct, 0, len(products))
	for _, product := range products {
		columns := make([]string, 0, len(product.Columns))
		for column := range product.Columns {
			columns = append(columns, column)
		}
		aggregates := make([]string, 0, len(product.AllowedAggregates))
		for aggregate := range product.AllowedAggregates {
			aggregates = append(aggregates, aggregate)
		}
		sort.Strings(columns)
		sort.Strings(aggregates)
		prompt = append(prompt, promptProduct{Name: product.Name, Columns: columns, Aggregates: aggregates})
	}
	encoded, err := json.Marshal(map[string]any{"products": prompt})
	return string(encoded), err
}

func allowedColumn(column string, product Product) error {
	if !safeIdentifier(column) {
		return fmt.Errorf("invalid column %q", column)
	}
	if _, ok := product.Columns[column]; !ok {
		return fmt.Errorf("column %q is not approved", column)
	}
	return nil
}

func safeIdentifier(value string) bool { return identifier.MatchString(value) }

func quoteIdentifier(value string) string { return `"` + value + `"` }

func sqlLiteral(value any) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "NULL", nil
	case string:
		if len(typed) > 4096 || strings.IndexByte(typed, 0) >= 0 {
			return "", errors.New("string literal is invalid")
		}
		return "'" + strings.ReplaceAll(typed, "'", "''") + "'", nil
	case bool:
		if typed {
			return "TRUE", nil
		}
		return "FALSE", nil
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64), nil
	case json.Number:
		if _, err := strconv.ParseFloat(string(typed), 64); err != nil {
			return "", errors.New("numeric literal is invalid")
		}
		return string(typed), nil
	default:
		return "", fmt.Errorf("literal type %T is not supported", value)
	}
}
