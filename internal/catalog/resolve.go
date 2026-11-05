package catalog

import (
	"fmt"
	"sort"
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

func (c *Catalog) LookupSnapshotPublication(name string) (SnapshotPublication, bool) {
	if c == nil {
		return SnapshotPublication{}, false
	}
	for _, publication := range c.SnapshotPublications {
		if publication.Name == name {
			return publication, true
		}
	}
	return SnapshotPublication{}, false
}

func (c *Catalog) ApprovalRouteFor(sensitivity domain.Sensitivity) (ApprovalRoute, bool) {
	if c == nil {
		return ApprovalRoute{}, false
	}
	for _, route := range c.ApprovalRoutes {
		if route.Sensitivity == sensitivity && len(route.Products) == 0 && len(route.ScopeCondition) == 0 {
			return route, true
		}
	}
	return ApprovalRoute{}, false
}

// ApprovalRouteForProducts resolves an exact product-scoped route before the
// ordinary sensitivity fallback. If any requested product is named by a
// scoped route, only an exact set match is allowed; subset/superset mixtures
// cannot inherit a broader sensitivity route.
func (c *Catalog) ApprovalRouteForProducts(sensitivity domain.Sensitivity, names []string) (ApprovalRoute, bool) {
	if c == nil {
		return ApprovalRoute{}, false
	}
	normalized := append([]string(nil), names...)
	for index := range normalized {
		normalized[index] = strings.TrimSpace(normalized[index])
	}
	sort.Strings(normalized)
	scopedProductRequested := false
	for _, route := range c.ApprovalRoutes {
		if len(route.Products) == 0 || len(route.ScopeCondition) > 0 {
			continue
		}
		if sameCatalogStrings(route.Products, normalized) {
			if route.Sensitivity != sensitivity {
				return ApprovalRoute{}, false
			}
			return cloneApprovalRoute(route), true
		}
		if catalogStringsIntersect(route.Products, normalized) {
			scopedProductRequested = true
		}
	}
	if scopedProductRequested {
		return ApprovalRoute{}, false
	}
	return c.ApprovalRouteFor(sensitivity)
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

func (c *Catalog) ResolveTaskPolicy(names []string) (TaskPolicy, error) {
	return c.ResolveTaskPolicyForScope(names, nil)
}

// ResolveTaskPolicyForScope resolves the task policy for a request whose
// normalized mandatory scope is known: the unconditioned route for the
// products is found as before, then the first conditioned route for the same
// sensitivity and product set whose scope condition holds, in declared order,
// takes its place.
func (c *Catalog) ResolveTaskPolicyForScope(names []string, scope map[string][]string) (TaskPolicy, error) {
	products, sensitivity, err := c.ResolveProducts(names)
	if err != nil {
		return TaskPolicy{}, err
	}
	resolvedNames := make([]string, 0, len(products))
	for _, product := range products {
		resolvedNames = append(resolvedNames, product.Name)
	}
	route, found := c.ApprovalRouteForProducts(sensitivity, resolvedNames)
	if !found {
		return TaskPolicy{}, fmt.Errorf("%w: no route for sensitivity", ErrInvalidApprovalRoute)
	}
	for _, candidate := range c.ApprovalRoutes {
		if len(candidate.ScopeCondition) > 0 && candidate.routeKey() == route.routeKey() && candidate.Matches(scope) {
			route = cloneApprovalRoute(candidate)
			break
		}
	}
	profile, found := c.LookupBudgetProfile(route.BudgetProfile)
	if !found {
		return TaskPolicy{}, fmt.Errorf("%w: route budget profile is missing", ErrInvalidBudgetProfile)
	}
	budget := profile.Budget()
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
	if product.ViewContract != nil {
		contract := *product.ViewContract
		product.ViewContract = &contract
	}
	return product
}

func cloneApprovalRoute(route ApprovalRoute) ApprovalRoute {
	route.Products = append([]string(nil), route.Products...)
	if route.ScopeCondition != nil {
		condition := make(map[string][]string, len(route.ScopeCondition))
		for name, values := range route.ScopeCondition {
			condition[name] = append([]string(nil), values...)
		}
		route.ScopeCondition = condition
	}
	route.Chain = cloneApprovalChain(route.Chain)
	return route
}

func sameCatalogStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func catalogStringsIntersect(left, right []string) bool {
	for _, one := range left {
		for _, two := range right {
			if one == two {
				return true
			}
		}
	}
	return false
}
