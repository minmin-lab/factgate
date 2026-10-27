package catalog

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/domain"
)

var (
	identifierPattern     = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	configNamePattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	versionPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	secretRefPattern      = regexp.MustCompile(`^(?:env:)?[A-Z][A-Z0-9_]{1,127}$`)
	databaseNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	reportingViewPattern  = regexp.MustCompile(`^reporting\.[a-z_][a-z0-9_]*$`)
	ordinalSidecarPattern = regexp.MustCompile(`^taskgate_ordinal\.[a-z_][a-z0-9_]*$`)
	functionPattern       = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	sha256HexPattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Catalog V1 attests the generic PostgreSQL data_type reported by
// information_schema.columns. Precision, scale, length, domains, and array
// element types are deliberately outside this version, so accepting typmod
// syntax such as numeric(10,2) would promise an attestation we do not perform.
var attestedPostgreSQLTypes = map[string]struct{}{
	"bigint": {}, "boolean": {}, "bytea": {}, "character": {}, "character varying": {},
	"date": {}, "double precision": {}, "integer": {}, "json": {}, "jsonb": {},
	"numeric": {}, "real": {}, "smallint": {}, "text": {},
	"time with time zone": {}, "time without time zone": {},
	"timestamp with time zone": {}, "timestamp without time zone": {}, "uuid": {},
}

var safeOperators = map[string]struct{}{
	"=": {}, "<>": {}, "!=": {}, "<": {}, "<=": {}, ">": {}, ">=": {},
	"+": {}, "-": {}, "*": {}, "/": {}, "%": {}, "and": {}, "or": {},
	"not": {}, "like": {}, "ilike": {}, "in": {}, "between": {}, "is": {},
	"is not": {},
}

var forbiddenFunctions = map[string]struct{}{
	"dblink": {}, "lo_export": {}, "lo_import": {}, "pg_ls_dir": {},
	"pg_read_binary_file": {}, "pg_read_file": {}, "pg_sleep": {},
	"pg_terminate_backend": {}, "set_config": {},
}

const (
	exposureProfileV4 = "taskgate-exposure-v4"
	exposureProfileV5 = "taskgate-exposure-v5"
)

func (c *Catalog) Validate() error {
	return c.validate(nil)
}

// ValidateViewContractCandidates applies the complete Catalog validation
// policy while allowing explicitly named, first-time semantic View candidates
// to omit both snapshot_publication and view_contract. It exists only for the
// read-only contract generator: runtime Catalog loading always calls Validate.
func (c *Catalog) ValidateViewContractCandidates(names []string) error {
	candidates := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !identifierPattern.MatchString(name) {
			return fieldError("view_contract_candidates", "candidate names must be lowercase product identifiers", ErrInvalidViewContract)
		}
		if _, duplicate := candidates[name]; duplicate {
			return fieldError("view_contract_candidates", "candidate names must be unique", ErrInvalidViewContract)
		}
		candidates[name] = struct{}{}
	}
	return c.validate(candidates)
}

func (c *Catalog) validate(viewContractCandidates map[string]struct{}) error {
	if c == nil {
		return fieldError("catalog", "catalog is nil", ErrMissingField)
	}

	var problems ValidationErrors
	if !versionPattern.MatchString(c.CatalogVersion) {
		problems = append(problems, fieldError("catalog_version", "a version using letters, digits, '.', '_' or '-' is required", ErrMissingField))
	}
	if len(c.Sources) == 0 {
		problems = append(problems, fieldError("sources", "at least one source is required", ErrMissingField))
	}
	if c.DelegatedAuthorization != nil {
		problems = append(problems, validateDelegatedAuthorization("delegated_authorization", *c.DelegatedAuthorization)...)
	}
	if len(c.Products) == 0 {
		problems = append(problems, fieldError("products", "at least one product is required", ErrMissingField))
	}
	if len(c.Scopes) == 0 {
		problems = append(problems, fieldError("scopes", "at least one scope is required", ErrMissingField))
	}
	if len(c.ApprovalRoutes) == 0 {
		problems = append(problems, fieldError("approval_routes", "at least one route is required", ErrMissingField))
	}
	if len(c.BudgetProfiles) == 0 {
		problems = append(problems, fieldError("budget_profiles", "at least one profile is required", ErrMissingField))
	}

	sources := make(map[string]struct{}, len(c.Sources))
	datasources := make(map[string]struct{}, len(c.Sources))
	for index, source := range c.Sources {
		path := fmt.Sprintf("sources[%d]", index)
		problems = append(problems, validateSource(path, source)...)
		if source.Name != "" {
			if _, exists := sources[source.Name]; exists {
				problems = append(problems, fieldError(path+".name", "source name is duplicated", ErrDuplicateSource))
			}
			sources[source.Name] = struct{}{}
		}
		if source.DatasourceID != "" {
			if _, exists := datasources[source.DatasourceID]; exists {
				problems = append(problems, fieldError(path+".datasource_id", "datasource_id is duplicated", ErrDuplicateSource))
			}
			datasources[source.DatasourceID] = struct{}{}
		}
	}

	publications := make(map[string]SnapshotPublication, len(c.SnapshotPublications))
	publicationBindings := make(map[string]struct{}, len(c.SnapshotPublications))
	sidecars := make(map[string]struct{}, len(c.SnapshotPublications))
	for index, publication := range c.SnapshotPublications {
		path := fmt.Sprintf("snapshot_publications[%d]", index)
		problems = append(problems, validateSnapshotPublication(path, publication, sources)...)
		if publication.Name != "" {
			if _, duplicate := publications[publication.Name]; duplicate {
				problems = append(problems, fieldError(path+".name", "snapshot publication name is duplicated", ErrInvalidSnapshotPublication))
			}
			publications[publication.Name] = publication
		}
		binding := publication.Source + "\x00" + publication.SourceNamespace + "\x00" + publication.Snapshot
		if _, duplicate := publicationBindings[binding]; duplicate {
			problems = append(problems, fieldError(path, "source/namespace/snapshot is published more than once", ErrInvalidSnapshotPublication))
		}
		publicationBindings[binding] = struct{}{}
		if _, duplicate := sidecars[publication.OrdinalSidecar]; duplicate {
			problems = append(problems, fieldError(path+".ordinal_sidecar", "ordinal sidecar is published more than once", ErrInvalidSnapshotPublication))
		}
		sidecars[publication.OrdinalSidecar] = struct{}{}
	}

	scopes := make(map[string]struct{}, len(c.Scopes))
	for index, scope := range c.Scopes {
		path := fmt.Sprintf("scopes[%d]", index)
		problems = append(problems, validateScope(path, scope)...)
		if scope.Name != "" {
			if _, exists := scopes[scope.Name]; exists {
				problems = append(problems, fieldError(path+".name", "scope name is duplicated", ErrInvalidCatalog))
			}
			scopes[scope.Name] = struct{}{}
		}
	}

	profiles := make(map[string]BudgetProfile, len(c.BudgetProfiles))
	for index, profile := range c.BudgetProfiles {
		path := fmt.Sprintf("budget_profiles[%d]", index)
		problems = append(problems, validateBudgetProfile(path, profile)...)
		if profile.Name != "" {
			if _, exists := profiles[profile.Name]; exists {
				problems = append(problems, fieldError(path+".name", "budget profile name is duplicated", ErrInvalidBudgetProfile))
			}
			profiles[profile.Name] = profile
		}
	}

	routes := make(map[domain.Sensitivity]ApprovalRoute, len(c.ApprovalRoutes))
	scopedRouteSets := make(map[string]struct{}, len(c.ApprovalRoutes))
	scopeDefinitions := make(map[string]Scope, len(c.Scopes))
	for _, scope := range c.Scopes {
		scopeDefinitions[scope.Name] = scope
	}
	unconditioned := make(map[string]struct{}, len(c.ApprovalRoutes))
	conditions := make(map[string]struct{}, len(c.ApprovalRoutes))
	for _, route := range c.ApprovalRoutes {
		if len(route.ScopeCondition) == 0 {
			unconditioned[route.routeKey()] = struct{}{}
		}
	}
	for index, route := range c.ApprovalRoutes {
		path := fmt.Sprintf("approval_routes[%d]", index)
		problems = append(problems, validateApprovalRoute(path, route, profiles)...)
		if route.Chain != nil {
			problems = append(problems, validateApprovalChain(path+".chain", *route.Chain, scopeDefinitions)...)
		}
		if len(route.ScopeCondition) > 0 {
			problems = append(problems, validateScopeCondition(path+".scope_condition", route.ScopeCondition, scopeDefinitions)...)
			if _, ok := unconditioned[route.routeKey()]; !ok {
				problems = append(problems, fieldError(path+".scope_condition", "a conditioned route needs an unconditioned route for the same sensitivity and products", ErrInvalidApprovalRoute))
			}
			condition, _ := CanonicalDeclarationDigest(route.ScopeCondition)
			if _, duplicate := conditions[route.routeKey()+"\x00"+condition]; duplicate {
				problems = append(problems, fieldError(path+".scope_condition", "conditioned approval route is duplicated", ErrInvalidApprovalRoute))
			}
			conditions[route.routeKey()+"\x00"+condition] = struct{}{}
			continue
		}
		if route.Sensitivity != "" && len(route.Products) == 0 {
			if _, exists := routes[route.Sensitivity]; exists {
				problems = append(problems, fieldError(path+".sensitivity", "approval route is duplicated", ErrInvalidApprovalRoute))
			}
			routes[route.Sensitivity] = route
		} else if route.Sensitivity != "" {
			key := strings.Join(route.Products, "\x00")
			if _, exists := scopedRouteSets[key]; exists {
				problems = append(problems, fieldError(path+".products", "product-scoped approval route is duplicated", ErrInvalidApprovalRoute))
			}
			scopedRouteSets[key] = struct{}{}
		}
	}
	ordinalDeployment := len(c.SnapshotPublications) != 0
	ordinalProfile := ""
	for _, route := range c.ApprovalRoutes {
		if profile, found := profiles[route.BudgetProfile]; found &&
			(profile.ExposureProfileVersion == exposureProfileV4 || profile.ExposureProfileVersion == exposureProfileV5) {
			ordinalDeployment = true
			if ordinalProfile == "" {
				ordinalProfile = profile.ExposureProfileVersion
			} else if ordinalProfile != profile.ExposureProfileVersion {
				problems = append(problems, fieldError("approval_routes",
					"V4 and V5 approval routes cannot coexist in one deployment", ErrInvalidApprovalRoute))
			}
		}
	}
	if ordinalDeployment {
		if len(c.SnapshotPublications) == 0 {
			problems = append(problems, fieldError("snapshot_publications",
				"an ordinal V4/V5 deployment requires at least one immutable snapshot publication", ErrInvalidSnapshotPublication))
		}
		for index, route := range c.ApprovalRoutes {
			if profile, found := profiles[route.BudgetProfile]; found &&
				(profile.ExposureProfileVersion != exposureProfileV4 && profile.ExposureProfileVersion != exposureProfileV5 ||
					ordinalProfile != "" && profile.ExposureProfileVersion != ordinalProfile) {
				problems = append(problems, fieldError(fmt.Sprintf("approval_routes[%d].budget_profile", index),
					"one ordinal profile and legacy/resource-only approval routes cannot coexist in one deployment", ErrInvalidApprovalRoute))
			}
		}
	}

	products := make(map[string]Product, len(c.Products))
	views := make(map[string]struct{}, len(c.Products))
	for index, product := range c.Products {
		path := fmt.Sprintf("products[%d]", index)
		problems = append(problems, validateProduct(path, product, sources, scopes, publications)...)
		if product.Name != "" {
			if _, exists := products[product.Name]; exists {
				problems = append(problems, fieldError(path+".name", "product name is duplicated", ErrDuplicateProduct))
			}
			products[product.Name] = product
		}
		if product.ReportingView != "" {
			if _, exists := views[product.ReportingView]; exists {
				problems = append(problems, fieldError(path+".reporting_view", "reporting view is published more than once", ErrInvalidReportingView))
			}
			views[product.ReportingView] = struct{}{}
		}
		if sensitivity, err := product.EffectiveSensitivity(); err == nil {
			route, exists := routes[sensitivity]
			if !exists {
				problems = append(problems, fieldError(path+".sensitivity", "no approval route exists for the effective sensitivity", ErrInvalidApprovalRoute))
			} else if profile, found := profiles[route.BudgetProfile]; found &&
				(profile.ExposureProfileVersion == exposureProfileV4 || profile.ExposureProfileVersion == exposureProfileV5) &&
				product.SnapshotPublication == "" && product.ViewContract == nil {
				if _, candidate := viewContractCandidates[product.Name]; !candidate {
					problems = append(problems, fieldError(path+".snapshot_publication",
						"V4 products require either a snapshot publication or a semantic View contract", ErrInvalidSnapshotPublication))
				}
			}
		}
	}
	for index, product := range c.Products {
		if product.Eligibility == nil {
			continue
		}
		problems = append(problems, validateEligibility(fmt.Sprintf("products[%d].eligibility", index), product.Eligibility, product.Scopes, c.Scopes)...)
	}
	problems = append(problems, c.validateKnowledgeProducts()...)
	if c.DelegatedAuthorization != nil {
		problems = append(problems, validateDelegatedScopes("delegated_authorization", *c.DelegatedAuthorization, scopeDefinitions)...)
	}
	problems = append(problems, c.validateStewardDepartments(scopeDefinitions)...)
	scopedProducts := make(map[string]string)
	for index, route := range c.ApprovalRoutes {
		// A conditioned route shares its fallback's product set and is
		// checked through it.
		if len(route.Products) == 0 || len(route.ScopeCondition) > 0 {
			continue
		}
		path := fmt.Sprintf("approval_routes[%d].products", index)
		var sensitivities []domain.Sensitivity
		for _, name := range route.Products {
			product, exists := products[name]
			if !exists {
				problems = append(problems, fieldError(path, "product-scoped route references an unknown product", ErrInvalidApprovalRoute))
				continue
			}
			sensitivity, sensitivityErr := product.EffectiveSensitivity()
			if sensitivityErr == nil {
				sensitivities = append(sensitivities, sensitivity)
			}
			if previous, exists := scopedProducts[name]; exists {
				problems = append(problems, fieldError(path, "product is already exclusive to "+previous, ErrInvalidApprovalRoute))
			} else {
				scopedProducts[name] = path
			}
		}
		if highest, highestErr := domain.HighestSensitivity(sensitivities...); highestErr == nil && highest != route.Sensitivity {
			problems = append(problems, fieldError(path, "route sensitivity differs from the exact product set", ErrInvalidApprovalRoute))
		}
	}
	if len(problems) > 0 {
		return problems
	}
	return nil
}

func validateSnapshotPublication(path string, publication SnapshotPublication, sources map[string]struct{}) ValidationErrors {
	var problems ValidationErrors
	if !configNamePattern.MatchString(publication.Name) {
		problems = append(problems, fieldError(path+".name", "a lowercase publication name is required", ErrInvalidSnapshotPublication))
	}
	if _, exists := sources[publication.Source]; !exists {
		problems = append(problems, fieldError(path+".source", "referenced source does not exist", ErrInvalidSnapshotPublication))
	}
	if strings.TrimSpace(publication.SourceNamespace) == "" || publication.SourceNamespace != strings.TrimSpace(publication.SourceNamespace) ||
		strings.ContainsAny(publication.SourceNamespace, "\x00\r\n\t") {
		problems = append(problems, fieldError(path+".source_namespace", "a canonical source namespace is required", ErrInvalidSnapshotPublication))
	}
	if !versionPattern.MatchString(publication.Snapshot) {
		problems = append(problems, fieldError(path+".snapshot", "a versioned immutable snapshot is required", ErrInvalidSnapshotPublication))
	}
	if !ordinalSidecarPattern.MatchString(publication.OrdinalSidecar) {
		problems = append(problems, fieldError(path+".ordinal_sidecar", "sidecar must be an unquoted taskgate_ordinal.<name> identifier", ErrInvalidSnapshotPublication))
	}
	for field, digest := range map[string]string{"sidecar_digest": publication.SidecarDigest,
		"dictionary_digest": publication.DictionaryDigest, "manifest_digest": publication.ManifestDigest} {
		if !sha256HexPattern.MatchString(digest) {
			problems = append(problems, fieldError(path+"."+field, field+" must be lowercase SHA-256", ErrInvalidSnapshotPublication))
		}
	}
	return problems
}

func validateSource(path string, source Source) ValidationErrors {
	var problems ValidationErrors
	if !identifierPattern.MatchString(source.Name) {
		problems = append(problems, fieldError(path+".name", "a lowercase logical name is required", ErrMissingField))
	}
	if !configNamePattern.MatchString(source.DatasourceID) {
		problems = append(problems, fieldError(path+".datasource_id", "a stable lowercase datasource_id is required", ErrMissingField))
	}
	if source.Type != "postgres" && source.Type != "postgresql" {
		problems = append(problems, fieldError(path+".type", "type must be postgres", ErrInvalidCatalog))
	}
	if strings.TrimSpace(source.Address) == "" || strings.ContainsAny(source.Address, " /@\t\r\n") || containsUserPassword(source.Address) {
		problems = append(problems, fieldError(path+".address", "a host address without credentials is required", ErrInvalidCatalog))
	}
	if source.Port < 1 || source.Port > 65535 {
		problems = append(problems, fieldError(path+".port", "port must be between 1 and 65535", ErrInvalidCatalog))
	}
	if !databaseNamePattern.MatchString(source.Database) {
		problems = append(problems, fieldError(path+".database", "a database name is required", ErrMissingField))
	}
	if !databaseNamePattern.MatchString(source.User) {
		problems = append(problems, fieldError(path+".user", "a database user is required", ErrMissingField))
	}
	if source.PostgreSQLMajorVersion <= 0 {
		problems = append(problems, fieldError(path+".postgres_major_version", "PostgreSQL major version is required", ErrMissingField))
	}
	if source.SchemaDigest != "" && !sha256HexPattern.MatchString(source.SchemaDigest) {
		problems = append(problems, fieldError(path+".schema_digest", "schema_digest must be lowercase SHA-256", ErrInvalidCatalog))
	}
	if source.Password != "" || source.DSN != "" {
		problems = append(problems, fieldError(path, "plaintext credentials and DSNs are forbidden; use secretRef", ErrPlaintextPassword))
	}
	if strings.TrimSpace(source.SecretRef) == "" {
		problems = append(problems, fieldError(path+".secretRef", "secretRef is required", ErrMissingSecretRef))
	} else if !secretRefPattern.MatchString(source.SecretRef) {
		problems = append(problems, fieldError(path+".secretRef", "secretRef must name an environment variable", ErrInvalidSecretRef))
	}
	return problems
}

func validateScope(path string, scope Scope) ValidationErrors {
	var problems ValidationErrors
	if !identifierPattern.MatchString(scope.Name) {
		problems = append(problems, fieldError(path+".name", "a lowercase scope name is required", ErrMissingField))
	}
	if strings.TrimSpace(scope.Description) == "" {
		problems = append(problems, fieldError(path+".description", "description is required", ErrMissingField))
	}
	switch scope.Type {
	case ScopeTypeEnum:
		if len(scope.AllowedValues) == 0 {
			problems = append(problems, fieldError(path+".allowed_values", "enum scope requires allowed values", ErrMissingField))
		}
		if duplicateOrEmpty(scope.AllowedValues) {
			problems = append(problems, fieldError(path+".allowed_values", "values must be non-empty and unique", ErrInvalidCatalog))
		}
		if scope.Min != "" || scope.Max != "" {
			problems = append(problems, fieldError(path, "enum scope cannot define min or max", ErrInvalidCatalog))
		}
	case ScopeTypeDateRange:
		minimum, minErr := time.Parse("2006-01-02", scope.Min)
		maximum, maxErr := time.Parse("2006-01-02", scope.Max)
		if minErr != nil || maxErr != nil {
			problems = append(problems, fieldError(path, "date_range scope requires min and max in YYYY-MM-DD form", ErrInvalidCatalog))
		} else if minimum.After(maximum) {
			problems = append(problems, fieldError(path, "date_range min is after max", ErrInvalidCatalog))
		}
		if len(scope.AllowedValues) != 0 {
			problems = append(problems, fieldError(path+".allowed_values", "date_range scope cannot define allowed values", ErrInvalidCatalog))
		}
	default:
		problems = append(problems, fieldError(path+".type", "type must be enum or date_range", ErrMissingField))
	}
	return problems
}

func validateBudgetProfile(path string, profile BudgetProfile) ValidationErrors {
	var problems ValidationErrors
	if !configNamePattern.MatchString(profile.Name) {
		problems = append(problems, fieldError(path+".name", "a lowercase profile name is required", ErrMissingField))
	}
	if err := profile.Budget().Validate(); err != nil {
		problems = append(problems, fieldError(path, err.Error(), ErrInvalidBudgetProfile))
	}
	if profile.ExposureProfileVersion != "" && profile.ExposureProfileVersion != "taskgate-exposure-v2" && profile.ExposureProfileVersion != "taskgate-exposure-v3" && profile.ExposureProfileVersion != "taskgate-exposure-v4" && profile.ExposureProfileVersion != "taskgate-exposure-v5" {
		problems = append(problems, fieldError(path+".exposure_profile_version", "catalog profiles must use taskgate-exposure-v2, taskgate-exposure-v3, taskgate-exposure-v4, or taskgate-exposure-v5", ErrInvalidBudgetProfile))
	}
	return problems
}

// validateScopeCondition requires every condition to name a Catalog enum scope
// and list sorted, distinct values that scope admits.
func validateScopeCondition(path string, condition map[string][]string, scopes map[string]Scope) ValidationErrors {
	var problems ValidationErrors
	for name, values := range condition {
		scope, ok := scopes[name]
		if !ok || scope.Type != ScopeTypeEnum {
			problems = append(problems, fieldError(path+"."+name, "a scope condition must name a Catalog enum scope", ErrInvalidApprovalRoute))
			continue
		}
		allowed := make(map[string]struct{}, len(scope.AllowedValues))
		for _, value := range scope.AllowedValues {
			allowed[value] = struct{}{}
		}
		if len(values) == 0 {
			problems = append(problems, fieldError(path+"."+name, "a scope condition lists at least one value", ErrInvalidApprovalRoute))
		}
		for index, value := range values {
			if _, ok := allowed[value]; !ok || index > 0 && values[index-1] >= value {
				problems = append(problems, fieldError(path+"."+name, "scope condition values must be sorted, distinct values of the scope", ErrInvalidApprovalRoute))
				break
			}
		}
	}
	return problems
}

func validateApprovalRoute(path string, route ApprovalRoute, profiles map[string]BudgetProfile) ValidationErrors {
	var problems ValidationErrors
	if err := route.Sensitivity.Validate(); err != nil {
		problems = append(problems, fieldError(path+".sensitivity", "a supported sensitivity is required", ErrInvalidApprovalRoute))
	}
	if route.Mode != domain.ApprovalModeManual {
		problems = append(problems, fieldError(path+".mode", "mode must be manual; automatic task approval is disabled", ErrInvalidApprovalRoute))
	}
	if route.Chain != nil {
		if strings.TrimSpace(route.Approver) != "" || len(route.ScopeCondition) > 0 {
			problems = append(problems, fieldError(path+".chain", "a chain route names neither an approver nor a scope condition", ErrInvalidApprovalRoute))
		}
	} else if strings.TrimSpace(route.Approver) == "" {
		problems = append(problems, fieldError(path+".approver", "manual route requires an approver", ErrInvalidApprovalRoute))
	}
	if strings.TrimSpace(route.BudgetProfile) == "" {
		problems = append(problems, fieldError(path+".budget_profile", "budget profile is required", ErrInvalidApprovalRoute))
	} else if _, exists := profiles[route.BudgetProfile]; !exists {
		problems = append(problems, fieldError(path+".budget_profile", "referenced budget profile does not exist", ErrInvalidApprovalRoute))
	}
	for index, product := range route.Products {
		if !identifierPattern.MatchString(product) || index > 0 && route.Products[index-1] >= product {
			problems = append(problems, fieldError(path+".products", "products must be sorted, unique lowercase logical names", ErrInvalidApprovalRoute))
			break
		}
	}
	return problems
}

func validateProduct(path string, product Product, sources, scopes map[string]struct{}, publications map[string]SnapshotPublication) ValidationErrors {
	var problems ValidationErrors
	v2Product := product.FactNamespace != "" || product.StableRelationRole != ""
	if !identifierPattern.MatchString(product.Name) {
		problems = append(problems, fieldError(path+".name", "a lowercase logical name is required", ErrMissingField))
	}
	if _, exists := sources[product.Source]; !exists {
		problems = append(problems, fieldError(path+".source", "referenced source does not exist", ErrInvalidCatalog))
	}
	if !reportingViewPattern.MatchString(product.ReportingView) {
		problems = append(problems, fieldError(path+".reporting_view", "view must be an unquoted reporting.<name> identifier", ErrInvalidReportingView))
	}
	if strings.TrimSpace(product.Description) == "" {
		problems = append(problems, fieldError(path+".description", "description is required", ErrMissingField))
	}
	if err := product.Sensitivity.Validate(); err != nil {
		problems = append(problems, fieldError(path+".sensitivity", "a supported sensitivity is required", ErrInvalidCatalog))
	}
	if len(product.Fields) == 0 {
		problems = append(problems, fieldError(path+".fields", "at least one field is required", ErrMissingField))
	}
	fieldNames := make(map[string]struct{}, len(product.Fields))
	for index, field := range product.Fields {
		fieldPath := fmt.Sprintf("%s.fields[%d]", path, index)
		if !identifierPattern.MatchString(field.Name) {
			problems = append(problems, fieldError(fieldPath+".name", "a lowercase field name is required", ErrMissingField))
		}
		if _, exists := fieldNames[field.Name]; exists {
			problems = append(problems, fieldError(fieldPath+".name", "field name is duplicated", ErrDuplicateField))
		}
		fieldNames[field.Name] = struct{}{}
		if _, supported := attestedPostgreSQLTypes[strings.ToLower(strings.TrimSpace(field.Type))]; !supported {
			problems = append(problems, fieldError(fieldPath+".type", "a supported generic PostgreSQL data_type without modifiers is required", ErrInvalidCatalog))
		}
		fieldType := strings.ToLower(strings.TrimSpace(field.Type))
		collatable := fieldType == "text" || fieldType == "character" || fieldType == "character varying"
		if v2Product && collatable && (strings.TrimSpace(field.Collation) == "" || strings.TrimSpace(field.CollationVersion) == "") {
			problems = append(problems, fieldError(fieldPath+".collation", "V2 collatable fields require an exact deterministic collation name and version", ErrInvalidCatalog))
		}
		if (!collatable || !v2Product) && (field.Collation != "" || field.CollationVersion != "") {
			problems = append(problems, fieldError(fieldPath+".collation", "collation metadata is allowed only on collatable V2 fields", ErrInvalidCatalog))
		}
		if strings.ContainsAny(field.Collation+field.CollationVersion, "\x00\r\n\t") || field.Collation != strings.TrimSpace(field.Collation) || field.CollationVersion != strings.TrimSpace(field.CollationVersion) {
			problems = append(problems, fieldError(fieldPath+".collation", "collation metadata must be canonical tokens", ErrInvalidCatalog))
		}
		if v2Product && fieldType == "time with time zone" {
			problems = append(problems, fieldError(fieldPath+".type", "time with time zone is outside taskgate-exposure-v2", ErrInvalidCatalog))
		}
		if strings.TrimSpace(field.Description) == "" {
			problems = append(problems, fieldError(fieldPath+".description", "description is required", ErrMissingField))
		}
		if field.Sensitivity != "" {
			if err := field.Sensitivity.Validate(); err != nil {
				problems = append(problems, fieldError(fieldPath+".sensitivity", "unsupported sensitivity", ErrInvalidCatalog))
			}
		}
	}
	if strings.TrimSpace(product.Snapshot) == "" {
		problems = append(problems, fieldError(path+".snapshot", "a versioned data snapshot is required", ErrMissingField))
	}
	if product.FactNamespace != "" && (strings.TrimSpace(product.FactNamespace) != product.FactNamespace || strings.ContainsAny(product.FactNamespace, "\x00\r\n\t")) {
		problems = append(problems, fieldError(path+".fact_namespace", "fact namespace must be a stable non-whitespace semantic identifier", ErrInvalidCatalog))
	}
	if v2Product && (product.FactNamespace == "" || product.StableRelationRole == "") {
		problems = append(problems, fieldError(path+".fact_namespace", "V2 products require both fact_namespace and stable_relation_role", ErrInvalidCatalog))
	}
	if product.StableRelationRole != "" && !configNamePattern.MatchString(product.StableRelationRole) {
		problems = append(problems, fieldError(path+".stable_relation_role", "stable relation role must be a lowercase catalog identifier", ErrInvalidCatalog))
	}
	if product.LineageManifestDigest != "" && !sha256HexPattern.MatchString(product.LineageManifestDigest) {
		problems = append(problems, fieldError(path+".lineage_manifest_digest", "lineage manifest digest must be lowercase SHA-256", ErrInvalidCatalog))
	}
	if product.ViewContract != nil {
		contract := *product.ViewContract
		contractPath := path + ".view_contract"
		if contract.ProfileVersion != ViewContractV1 {
			problems = append(problems, fieldError(contractPath+".profile_version", "profile_version must be "+ViewContractV1, ErrInvalidViewContract))
		}
		for field, digest := range map[string]string{
			"definition_digest":     contract.DefinitionDigest,
			"dependency_digest":     contract.DependencyDigest,
			"canonical_plan_digest": contract.CanonicalPlanDigest,
			"interface_digest":      contract.InterfaceDigest,
		} {
			if !sha256HexPattern.MatchString(digest) {
				problems = append(problems, fieldError(contractPath+"."+field, field+" must be lowercase SHA-256", ErrInvalidViewContract))
			}
		}
		if product.FactNamespace == "" || product.StableRelationRole == "" {
			problems = append(problems, fieldError(contractPath, "semantic View products require stable fact_namespace and stable_relation_role", ErrInvalidViewContract))
		}
		if product.SnapshotPublication != "" {
			problems = append(problems, fieldError(contractPath, "expandable semantic Views cannot also be opaque snapshot publications", ErrInvalidViewContract))
		}
	}
	if product.SnapshotPublication != "" {
		publication, exists := publications[product.SnapshotPublication]
		if !exists {
			problems = append(problems, fieldError(path+".snapshot_publication", "referenced snapshot publication does not exist", ErrInvalidSnapshotPublication))
		} else if publication.Source != product.Source || publication.SourceNamespace != product.FactNamespace || publication.Snapshot != product.Snapshot {
			problems = append(problems, fieldError(path+".snapshot_publication", "publication source, namespace, and snapshot must match the product", ErrInvalidSnapshotPublication))
		}
	}
	if len(product.EntityKey) == 0 || duplicateOrEmpty(product.EntityKey) {
		problems = append(problems, fieldError(path+".entity_key", "at least one unique entity key field is required", ErrMissingField))
	}
	for _, key := range product.EntityKey {
		if _, published := fieldNames[key]; !published {
			problems = append(problems, fieldError(path+".entity_key", "entity key must name a published product field", ErrInvalidCatalog))
		}
	}
	if len(product.Scopes) == 0 {
		problems = append(problems, fieldError(path+".scopes", "at least one mandatory scope is required", ErrMissingField))
	}
	if duplicateOrEmpty(product.Scopes) {
		problems = append(problems, fieldError(path+".scopes", "scope names must be non-empty and unique", ErrInvalidCatalog))
	}
	for _, scope := range product.Scopes {
		if _, exists := scopes[scope]; !exists {
			problems = append(problems, fieldError(path+".scopes", "referenced scope does not exist", ErrInvalidCatalog))
		}
		if _, published := fieldNames[scope]; !published {
			problems = append(problems, fieldError(path+".scopes", "mandatory scope must name a published product field", ErrInvalidCatalog))
		}
	}
	problems = append(problems, validateFunctions(path+".allowed_functions", product.AllowedFunctions)...)
	problems = append(problems, validateFunctions(path+".allowed_aggregates", product.AllowedAggregates)...)
	if v2Product {
		for _, aggregate := range product.AllowedAggregates {
			switch strings.ToLower(strings.TrimSpace(aggregate)) {
			case "count", "sum", "min", "max", "avg":
			default:
				problems = append(problems, fieldError(path+".allowed_aggregates", "V2 supports only count, sum, min, max, and avg", ErrInvalidCatalog))
			}
		}
	}
	for _, operator := range product.AllowedOperators {
		if _, ok := safeOperators[strings.ToLower(strings.TrimSpace(operator))]; !ok {
			problems = append(problems, fieldError(path+".allowed_operators", "operator is not in the safe catalog vocabulary", ErrInvalidCatalog))
		}
	}
	if duplicateOrEmpty(product.AllowedOperators) {
		problems = append(problems, fieldError(path+".allowed_operators", "operators must be non-empty and unique", ErrInvalidCatalog))
	}
	return problems
}

func validateFunctions(path string, functions []string) ValidationErrors {
	var problems ValidationErrors
	for _, function := range functions {
		if !functionPattern.MatchString(function) {
			problems = append(problems, fieldError(path, "function must be an unqualified lowercase identifier", ErrInvalidCatalog))
			continue
		}
		if _, forbidden := forbiddenFunctions[function]; forbidden {
			problems = append(problems, fieldError(path, "dangerous function cannot be published", ErrInvalidCatalog))
		}
	}
	if duplicateOrEmpty(functions) {
		problems = append(problems, fieldError(path, "functions must be non-empty and unique", ErrInvalidCatalog))
	}
	return problems
}

func duplicateOrEmpty(values []string) bool {
	if len(values) == 0 {
		return false
	}
	copyValues := append([]string(nil), values...)
	for index := range copyValues {
		copyValues[index] = strings.TrimSpace(copyValues[index])
		if copyValues[index] == "" {
			return true
		}
	}
	sort.Strings(copyValues)
	for index := 1; index < len(copyValues); index++ {
		if copyValues[index] == copyValues[index-1] {
			return true
		}
	}
	return false
}

func validateAttributeCondition(path string, condition AttributeCondition) ValidationErrors {
	var problems ValidationErrors
	if !identifierPattern.MatchString(condition.Attribute) {
		problems = append(problems, fieldError(path+".attribute", "a lowercase attribute name is required", ErrInvalidCatalog))
	}
	if len(condition.AnyOf) == 0 || duplicateOrEmpty(condition.AnyOf) {
		problems = append(problems, fieldError(path+".any_of", "any_of must list distinct non-empty values", ErrInvalidCatalog))
	}
	for _, value := range condition.AnyOf {
		if strings.ContainsAny(value, "\x00\r\n\t") || value != strings.TrimSpace(value) {
			problems = append(problems, fieldError(path+".any_of", "attribute values must be canonical tokens", ErrInvalidCatalog))
			break
		}
	}
	return problems
}

func validateEligibility(path string, eligibility *Eligibility, mandatoryScopes []string, scopes []Scope) ValidationErrors {
	var problems ValidationErrors
	if len(eligibility.Require) == 0 && len(eligibility.ScopeEnvelope) == 0 {
		problems = append(problems, fieldError(path, "eligibility must declare require conditions or a scope envelope", ErrInvalidCatalog))
	}
	for index, condition := range eligibility.Require {
		problems = append(problems, validateAttributeCondition(fmt.Sprintf("%s.require[%d]", path, index), condition)...)
	}
	productScopes := make(map[string]struct{}, len(mandatoryScopes))
	for _, name := range mandatoryScopes {
		productScopes[name] = struct{}{}
	}
	definitions := make(map[string]Scope, len(scopes))
	for _, scope := range scopes {
		definitions[scope.Name] = scope
	}
	for name, rule := range eligibility.ScopeEnvelope {
		rulePath := path + ".scope_envelope." + name
		if _, mandatory := productScopes[name]; !mandatory {
			problems = append(problems, fieldError(rulePath, "scope envelope must name a mandatory scope of this product", ErrInvalidCatalog))
		}
		if definition, ok := definitions[name]; ok && definition.Type != ScopeTypeEnum {
			problems = append(problems, fieldError(rulePath, "scope envelopes are defined for enum scopes only", ErrInvalidCatalog))
		}
		if !identifierPattern.MatchString(rule.FromAttribute) {
			problems = append(problems, fieldError(rulePath+".from_attribute", "a lowercase attribute name is required", ErrInvalidCatalog))
		}
		for index, condition := range rule.WidenIf {
			problems = append(problems, validateAttributeCondition(fmt.Sprintf("%s.widen_if[%d]", rulePath, index), condition)...)
		}
	}
	return problems
}
