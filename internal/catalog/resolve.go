package catalog

import (
	"fmt"
	"strings"

	"taskbound.local/agent-data-gateway/internal/domain"
)

// TaskPolicy is the deterministic catalog decision used to create an OA draft
// and, after approval, a TaskGrant.
type TaskPolicy struct {
	Products      []Product          `json:"products"`
	Sensitivity   domain.Sensitivity `json:"sensitivity"`
	ApprovalRoute ApprovalRoute      `json:"approval_route"`
	BudgetProfile string             `json:"budget_profile"`
	Budget        domain.Budget      `json:"budget"`
}

func (c *Catalog) LookupProduct(name string) (Product, bool) {
	if c == nil {
		return Product{}, false
	}
	for _, product := range c.Products {
		if product.Name == name {
			return cloneProduct(product), true
		}
	}
	return Product{}, false
}

func (c *Catalog) ListProducts() []Product {
	if c == nil {
		return nil
	}
	products := make([]Product, 0, len(c.Products))
	for _, product := range c.Products {
		products = append(products, cloneProduct(product))
	}
	return products
}

func (c *Catalog) ApprovalRouteFor(sensitivity domain.Sensitivity) (ApprovalRoute, bool) {
	if c == nil {
		return ApprovalRoute{}, false
	}
	for _, route := range c.ApprovalRoutes {
		if route.Sensitivity == sensitivity {
			return route, true
		}
	}
	return ApprovalRoute{}, false
}

func (c *Catalog) LookupBudgetProfile(name string) (BudgetProfile, bool) {
	if c == nil {
		return BudgetProfile{}, false
	}
	for _, profile := range c.BudgetProfiles {
		if profile.Name == name {
			return profile, true
		}
	}
	return BudgetProfile{}, false
}

// ResolveProducts validates requested logical names and computes their highest
// effective classification. An empty request is rejected rather than silently
// granting all products.
func (c *Catalog) ResolveProducts(names []string) ([]Product, domain.Sensitivity, error) {
	if c == nil {
		return nil, "", ErrInvalidCatalog
	}
	if len(names) == 0 {
		return nil, "", fmt.Errorf("%w: no products requested", ErrUnknownProduct)
	}
	seen := make(map[string]struct{}, len(names))
	products := make([]Product, 0, len(names))
	sensitivities := make([]domain.Sensitivity, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if _, duplicate := seen[name]; duplicate {
			return nil, "", fmt.Errorf("%w: duplicate requested product", ErrUnknownProduct)
		}
		seen[name] = struct{}{}
		product, found := c.LookupProduct(name)
		if !found {
			return nil, "", fmt.Errorf("%w: %q", ErrUnknownProduct, name)
		}
		sensitivity, err := product.EffectiveSensitivity()
		if err != nil {
			return nil, "", fmt.Errorf("%w: product sensitivity", ErrInvalidCatalog)
		}
		products = append(products, product)
		sensitivities = append(sensitivities, sensitivity)
	}
	highest, err := domain.HighestSensitivity(sensitivities...)
	if err != nil {
		return nil, "", err
	}
	return products, highest, nil
}

func (c *Catalog) ResolveTaskPolicy(names []string, request *domain.BudgetRequest) (TaskPolicy, error) {
	products, sensitivity, err := c.ResolveProducts(names)
	if err != nil {
		return TaskPolicy{}, err
	}
	route, found := c.ApprovalRouteFor(sensitivity)
	if !found {
		return TaskPolicy{}, fmt.Errorf("%w: no route for sensitivity", ErrInvalidApprovalRoute)
	}
	profile, found := c.LookupBudgetProfile(route.BudgetProfile)
	if !found {
		return TaskPolicy{}, fmt.Errorf("%w: route budget profile is missing", ErrInvalidBudgetProfile)
	}
	budget := profile.Budget()
	if request != nil {
		budget, err = request.Apply(budget)
		if err != nil {
			return TaskPolicy{}, fmt.Errorf("requested budget: %w", err)
		}
	}
	return TaskPolicy{
		Products:      products,
		Sensitivity:   sensitivity,
		ApprovalRoute: route,
		BudgetProfile: profile.Name,
		Budget:        budget,
	}, nil
}

func cloneProduct(product Product) Product {
	product.Fields = append([]Field(nil), product.Fields...)
	product.Scopes = append([]string(nil), product.Scopes...)
	product.AllowedFunctions = append([]string(nil), product.AllowedFunctions...)
	product.AllowedOperators = append([]string(nil), product.AllowedOperators...)
	product.AllowedAggregates = append([]string(nil), product.AllowedAggregates...)
	return product
}
