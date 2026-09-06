package catalog

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/domain"
)

func validCatalogYAML(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("read test catalog: %v", err)
	}
	return string(data)
}

func TestParseValidCatalogAndResolvePolicy(t *testing.T) {
	parsed, err := Parse([]byte(validCatalogYAML(t)))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if parsed.CatalogVersion != "2026.07.21" || len(parsed.Products) != 2 {
		t.Fatalf("unexpected catalog: %#v", parsed)
	}
	if parsed.V4Enabled() {
		t.Fatal("legacy catalog fixture unexpectedly selected V4 deployment mode")
	}
	if len(parsed.SHA256) != 64 {
		t.Fatalf("catalog SHA-256 = %q, want 64 lowercase hex characters", parsed.SHA256)
	}
	policy, err := parsed.ResolveTaskPolicy([]string{"expense_summary", "expense_detail"})
	if err != nil {
		t.Fatalf("ResolveTaskPolicy returned error: %v", err)
	}
	if policy.Sensitivity != domain.SensitivityHigh || policy.ApprovalRoute.Mode != domain.ApprovalModeManual {
		t.Fatalf("unexpected policy routing: %#v", policy)
	}
	if policy.Budget.MaxRows != 100 || policy.Budget.TaskTTL != 15*time.Minute {
		t.Fatalf("unexpected policy budget: %#v", policy.Budget)
	}
}

func TestCatalogTimeTZBoundaryMatchesExposureProfile(t *testing.T) {
	legacy := strings.Replace(validCatalogYAML(t), "        type: date\n", "        type: time with time zone\n", 1)
	if _, err := Parse([]byte(legacy)); err != nil {
		t.Fatalf("legacy Catalog rejected supported timetz result field: %v", err)
	}

	v4Data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatalf("read V4 Catalog: %v", err)
	}
	v4 := strings.Replace(string(v4Data), "        type: date\n", "        type: time with time zone\n", 1)
	if _, err := Parse([]byte(v4)); !errors.Is(err, ErrInvalidCatalog) ||
		!strings.Contains(err.Error(), "time with time zone is outside taskgate-exposure-v2") {
		t.Fatalf("V4 Catalog timetz error = %v, want exposure-domain rejection", err)
	}
}

func TestRepositoryCatalog(t *testing.T) {
	parsed, err := Load("../../config/catalog.yaml")
	if err != nil {
		t.Fatalf("repository catalog is not startup-safe: %v", err)
	}
	if _, _, err := parsed.ResolveProducts([]string{"expense_summary", "expense_detail"}); err != nil {
		t.Fatalf("repository products cannot be resolved: %v", err)
	}
	wantPublications := map[string]bool{
		"expense-summary-v1":         false,
		"expense-detail-v1":          false,
		"provsql-orders-v1":          false,
		"provsql-lineitem-v1":        false,
		"provsql-nonce-v1":           false,
		"final-v5-result-heavy-v1":   false,
		"final-v5-exposure-scale-v1": false,
		"final-v5-scale-e7-v1":       false,
	}
	if len(parsed.SnapshotPublications) != len(wantPublications) {
		t.Fatalf("repository snapshot publications = %d, want exact set of %d", len(parsed.SnapshotPublications), len(wantPublications))
	}
	for _, publication := range parsed.SnapshotPublications {
		if _, found := wantPublications[publication.Name]; !found {
			t.Fatalf("repository contains unexpected snapshot publication %q", publication.Name)
		}
		if wantPublications[publication.Name] {
			t.Fatalf("repository contains duplicate snapshot publication %q", publication.Name)
		}
		wantPublications[publication.Name] = true
	}
	for publication, found := range wantPublications {
		if !found {
			t.Fatalf("repository is missing snapshot publication %q", publication)
		}
	}
	if !parsed.V4Enabled() {
		t.Fatal("repository Catalog did not select V4 deployment mode")
	}
	publication, found := parsed.LookupSnapshotPublication("expense-detail-v1")
	if !found || publication.SourceNamespace != "travel.expense_receipt" || len(publication.ManifestDigest) != 64 {
		t.Fatalf("unexpected V4 publication: %#v, found=%v", publication, found)
	}
	policy, err := parsed.ResolveTaskPolicy([]string{"expense_detail"})
	if err != nil || policy.Budget.ExposureProfileVersion != "taskgate-exposure-v5" || policy.Budget.PredicateFootprint == nil {
		t.Fatalf("repository V5 policy = %#v, err=%v", policy, err)
	}
	// The three frozen ProvSQL relations carry no scoped route of their own, as
	// the reviewed Catalog candidate has it: Baseline S1 requests provsql_orders
	// alone and S2 requests it with provsql_lineitem, and a Product may hold at
	// most one scoped route, so all three closures resolve through the default
	// low route.
	for _, closure := range [][]string{
		{"provsql_orders"},
		{"provsql_lineitem", "provsql_orders"},
		{"provsql_lineitem", "provsql_nonce", "provsql_orders"},
	} {
		provsql, err := parsed.ResolveTaskPolicy(closure)
		if err != nil || provsql.BudgetProfile != "final-v5-baseline-low-v1" || provsql.Budget.MaxInfluenceFacts < 1_000_000 {
			t.Fatalf("repository closure %v policy = %#v, err=%v", closure, provsql, err)
		}
		// max_rows is a cumulative per-Task ledger and one Baseline Task carries
		// a cell's novel query plus its three replays, so anything below four
		// S1/SF10 results refuses the cell's third governed query.
		if provsql.Budget.MaxRows < 4*50_000 {
			t.Fatalf("closure %v grants %d rows; one S1/SF10 Task settles %d",
				closure, provsql.Budget.MaxRows, 4*50_000)
		}
	}
	// expense_summary deliberately carries no scoped route of its own, so it
	// shares the default low route. That is what keeps it requestable together
	// with expense_detail: a Product named by a scoped route can only ever be
	// requested as exactly that set.
	summary, err := parsed.ResolveTaskPolicy([]string{"expense_summary"})
	if err != nil || summary.BudgetProfile != "final-v5-baseline-low-v1" {
		t.Fatalf("repository expense_summary policy = %#v, err=%v", summary, err)
	}
	mixed, err := parsed.ResolveTaskPolicy([]string{"expense_summary", "expense_detail"})
	if err != nil || mixed.Sensitivity != domain.SensitivityHigh {
		t.Fatalf("summary and detail can no longer be requested together: %#v, err=%v", mixed, err)
	}
}

func TestProductScopedApprovalRouteIsExactAndFailClosed(t *testing.T) {
	parsed, err := Load("../../config/catalog.yaml")
	if err != nil {
		t.Fatalf("load repository Catalog: %v", err)
	}
	attack, err := parsed.ResolveTaskPolicy([]string{"final_v5_attack_expense_detail"})
	if err != nil {
		t.Fatalf("resolve exact Attack product route: %v", err)
	}
	if attack.BudgetProfile != "final-v5-attack-medium-v1" ||
		len(attack.ApprovalRoute.Products) != 1 || attack.ApprovalRoute.Products[0] != "final_v5_attack_expense_detail" {
		t.Fatalf("exact scoped policy = %#v", attack)
	}
	attack.ApprovalRoute.Products[0] = "mutated"
	again, err := parsed.ResolveTaskPolicy([]string{"final_v5_attack_expense_detail"})
	if err != nil || again.ApprovalRoute.Products[0] != "final_v5_attack_expense_detail" {
		t.Fatalf("scoped route slice escaped Catalog ownership: policy=%#v err=%v", again, err)
	}

	ordinary, err := parsed.ResolveTaskPolicy([]string{"expense_detail"})
	if err != nil {
		t.Fatalf("resolve ordinary sensitivity fallback: %v", err)
	}
	if ordinary.BudgetProfile != "detail-manual-v5" || len(ordinary.ApprovalRoute.Products) != 0 {
		t.Fatalf("ordinary fallback changed = %#v", ordinary)
	}

	if _, err := parsed.ResolveTaskPolicy([]string{"expense_summary", "final_v5_attack_expense_detail"}); !errors.Is(err, ErrInvalidApprovalRoute) {
		t.Fatalf("mixed scoped product error = %v, want ErrInvalidApprovalRoute", err)
	}
}

func TestConcurrencyProductUsesExactIndependentRouteAndBudget(t *testing.T) {
	parsed, err := Load("../../config/catalog.yaml")
	if err != nil {
		t.Fatalf("load repository Catalog: %v", err)
	}
	product, found := parsed.LookupProduct("final_v5_concurrency_expense_detail")
	if !found || product.ReportingView != "reporting.final_v5_concurrency_expense_detail" ||
		product.SnapshotPublication != "expense-detail-v1" || product.FactNamespace != "travel.expense_receipt" ||
		product.StableRelationRole != "expense_detail" || len(product.Fields) != 4 {
		t.Fatalf("dedicated concurrency product drifted: found=%v product=%#v", found, product)
	}
	policy, err := parsed.ResolveTaskPolicy([]string{"final_v5_concurrency_expense_detail"})
	if err != nil {
		t.Fatalf("resolve exact concurrency product route: %v", err)
	}
	if policy.BudgetProfile != "final-v5-concurrency-v1" || policy.Budget.MaxQueries != 600 ||
		policy.Budget.MaxRows < 504 || policy.Budget.MaxOutcomeFacts != 5 ||
		policy.Budget.ExposureProfileVersion != "taskgate-exposure-v5" ||
		len(policy.ApprovalRoute.Products) != 1 || policy.ApprovalRoute.Products[0] != product.Name {
		t.Fatalf("dedicated concurrency policy drifted: %#v", policy)
	}
	if _, err := parsed.ResolveTaskPolicy([]string{"final_v5_attack_expense_detail", product.Name}); !errors.Is(err, ErrInvalidApprovalRoute) {
		t.Fatalf("mixed evaluation products bypassed exact scoped route: %v", err)
	}
}

func TestProductScopedApprovalRouteRejectsInvalidCatalogBindings(t *testing.T) {
	data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	valid := string(data)
	const exact = "    products: [final_v5_attack_expense_detail]\n"
	const route = `  - sensitivity: medium
    products: [final_v5_attack_expense_detail]
    mode: manual
    approver: bob
    budget_profile: final-v5-attack-medium-v1
`
	tests := []struct {
		name string
		yaml string
	}{
		{"duplicate route", strings.Replace(valid, "\nbudget_profiles:\n", "\n"+route+"\nbudget_profiles:\n", 1)},
		{"duplicate product", strings.Replace(valid, exact, "    products: [final_v5_attack_expense_detail, final_v5_attack_expense_detail]\n", 1)},
		{"unsorted products", strings.Replace(valid, exact, "    products: [final_v5_attack_expense_detail, expense_summary]\n", 1)},
		{"unknown product", strings.Replace(valid, exact, "    products: [final_v5_attack_unknown]\n", 1)},
		{"sensitivity mismatch", strings.Replace(valid, route, strings.Replace(route, "sensitivity: medium", "sensitivity: high", 1), 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, parseErr := Parse([]byte(test.yaml)); !errors.Is(parseErr, ErrInvalidApprovalRoute) {
				t.Fatalf("Parse error = %v, want ErrInvalidApprovalRoute", parseErr)
			}
		})
	}
}

func TestParseViewContractCandidatesRelaxesOnlyMissingGeneratedContract(t *testing.T) {
	data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	candidate := bytes.Replace(data, []byte("    snapshot_publication: expense-summary-v1\n"), nil, 1)
	if bytes.Equal(candidate, data) {
		t.Fatal("fixture did not remove the candidate publication")
	}
	if _, err := Parse(candidate); !errors.Is(err, ErrInvalidSnapshotPublication) {
		t.Fatalf("strict Parse error = %v, want %v", err, ErrInvalidSnapshotPublication)
	}
	parsed, err := ParseViewContractCandidates(candidate, []string{"expense_summary"})
	if err != nil {
		t.Fatalf("candidate parse: %v", err)
	}
	if parsed.SHA256 == "" {
		t.Fatal("candidate parse omitted exact artifact digest")
	}
	if _, err := ParseViewContractCandidates(candidate, []string{"expense_detail"}); !errors.Is(err, ErrInvalidSnapshotPublication) {
		t.Fatalf("unselected missing contract error = %v, want %v", err, ErrInvalidSnapshotPublication)
	}
	if _, err := ParseViewContractCandidates(candidate, []string{"Expense Summary"}); !errors.Is(err, ErrInvalidViewContract) {
		t.Fatalf("invalid candidate name error = %v, want %v", err, ErrInvalidViewContract)
	}
}

func TestCatalogViewContractIsAllOrNothingAndCloned(t *testing.T) {
	data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	contract := "    view_contract:\n" +
		"      profile_version: taskgate-view-contract-v1\n" +
		"      definition_digest: " + digest + "\n" +
		"      dependency_digest: " + digest + "\n" +
		"      canonical_plan_digest: " + digest + "\n" +
		"      interface_digest: " + digest + "\n"
	withoutOpaquePublication := strings.Replace(string(data), "    snapshot_publication: expense-summary-v1\n", "", 1)
	withContract := strings.Replace(withoutOpaquePublication, "    stable_relation_role: expense_summary\n",
		"    stable_relation_role: expense_summary\n"+contract, 1)
	parsed, err := Parse([]byte(withContract))
	if err != nil {
		t.Fatalf("parse View contract: %v", err)
	}
	product, found := parsed.LookupProduct("expense_summary")
	if !found || product.ViewContract == nil || product.ViewContract.ProfileVersion != ViewContractV1 {
		t.Fatalf("missing parsed View contract: %#v", product.ViewContract)
	}
	product.ViewContract.DependencyDigest = strings.Repeat("b", 64)
	again, _ := parsed.LookupProduct("expense_summary")
	if again.ViewContract.DependencyDigest != digest {
		t.Fatal("LookupProduct returned an aliased View contract")
	}

	invalid := strings.Replace(withContract, "      interface_digest: "+digest,
		"      interface_digest: not-a-digest", 1)
	if _, err := Parse([]byte(invalid)); !errors.Is(err, ErrInvalidViewContract) {
		t.Fatalf("invalid View contract error = %v, want ErrInvalidViewContract", err)
	}

	withBoth := strings.Replace(withContract, "    stable_relation_role: expense_summary\n",
		"    stable_relation_role: expense_summary\n    snapshot_publication: expense-summary-v1\n", 1)
	if _, err := Parse([]byte(withBoth)); !errors.Is(err, ErrInvalidViewContract) {
		t.Fatalf("View contract plus opaque publication error = %v, want ErrInvalidViewContract", err)
	}
}

func TestV5CatalogRejectsMixedLegacyApprovalRoute(t *testing.T) {
	data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Make one reachable route legacy while the others stay V5, and keep the
	// mutated profile internally well-formed: a predicate footprint requires V5,
	// so leaving one behind would raise a budget error before the route mix is
	// ever examined. Targeting the profile by name rather than by first
	// occurrence keeps this test measuring the route rule when the Catalog's
	// budget order changes.
	const mutated = "final-v5-baseline-low-v1"
	profile := strings.Index(string(data), "  - name: "+mutated+"\n")
	if profile < 0 {
		t.Fatalf("Catalog no longer carries the %s budget profile", mutated)
	}
	end := strings.Index(string(data)[profile+1:], "\n  - name: ")
	if end < 0 {
		t.Fatal("could not bound the mutated budget profile")
	}
	block := string(data)[profile : profile+1+end]
	legacy := strings.Replace(block,
		"exposure_profile_version: taskgate-exposure-v5",
		"exposure_profile_version: taskgate-exposure-v3", 1)
	if footprint := strings.Index(legacy, "    predicate_footprint:"); footprint >= 0 {
		legacy = legacy[:footprint]
	}
	mixed := strings.Replace(string(data), block, legacy, 1)
	if _, err := Parse([]byte(mixed)); !errors.Is(err, ErrInvalidApprovalRoute) {
		t.Fatalf("mixed V5/legacy Catalog error = %v, want ErrInvalidApprovalRoute", err)
	}
}

func TestV4CatalogFailsClosedOnMissingOrMismatchedPublication(t *testing.T) {
	data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatalf("read repository catalog: %v", err)
	}
	valid := string(data)
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("parse repository catalog fixture: %v", err)
	}
	detailPublication, found := parsed.LookupSnapshotPublication("expense-detail-v1")
	if !found {
		t.Fatal("repository catalog omits expense-detail-v1")
	}
	tests := []struct {
		name string
		yaml string
	}{
		{"missing product binding", strings.Replace(valid, "    snapshot_publication: expense-detail-v1\n", "", 1)},
		{"mismatched namespace", strings.Replace(valid, "    source_namespace: travel.expense_receipt", "    source_namespace: travel.wrong", 1)},
		{"invalid manifest digest", strings.Replace(valid, detailPublication.ManifestDigest, "not-a-digest", 1)},
		{"unknown sidecar schema", strings.Replace(valid, "taskgate_ordinal.expense_detail_v1", "reporting.expense_detail_v1", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, parseErr := Parse([]byte(test.yaml))
			if !errors.Is(parseErr, ErrInvalidSnapshotPublication) {
				t.Fatalf("Parse error = %v, want ErrInvalidSnapshotPublication", parseErr)
			}
		})
	}
}

func TestCatalogRejectsRequiredInvalidInputs(t *testing.T) {
	valid := validCatalogYAML(t)
	tests := []struct {
		name   string
		yaml   string
		target error
	}{
		{
			name:   "missing field",
			yaml:   strings.Replace(valid, "catalog_version: \"2026.07.21\"\n", "", 1),
			target: ErrMissingField,
		},
		{
			name:   "duplicate product",
			yaml:   strings.Replace(valid, "name: expense_detail", "name: expense_summary", 1),
			target: ErrDuplicateProduct,
		},
		{
			name:   "illegal reporting view",
			yaml:   strings.Replace(valid, "reporting.expense_summary", "legacy.expense_summary", 1),
			target: ErrInvalidReportingView,
		},
		{
			name:   "plaintext password",
			yaml:   strings.Replace(valid, "    secretRef: env:EXPENSES_DB_PASSWORD", "    secretRef: env:EXPENSES_DB_PASSWORD\n    password: do-not-log-this", 1),
			target: ErrPlaintextPassword,
		},
		{
			name:   "missing secretRef",
			yaml:   strings.Replace(valid, "    secretRef: env:EXPENSES_DB_PASSWORD\n", "", 1),
			target: ErrMissingSecretRef,
		},
		{
			name:   "missing datasource id",
			yaml:   strings.Replace(valid, "    datasource_id: taskgate-test-expenses\n", "", 1),
			target: ErrMissingField,
		},
		{
			name:   "invalid schema digest",
			yaml:   strings.Replace(valid, "    schema_digest: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "    schema_digest: not-a-digest", 1),
			target: ErrInvalidCatalog,
		},
		{
			name: "duplicate datasource id",
			yaml: strings.Replace(valid, "    secretRef: env:EXPENSES_DB_PASSWORD\n", `    secretRef: env:EXPENSES_DB_PASSWORD
  - name: warehouse
    datasource_id: taskgate-test-expenses
    type: postgres
    address: warehouse
    port: 5432
    database: travel_demo
    user: gateway_reader
    postgres_major_version: 16
    secretRef: env:EXPENSES_DB_PASSWORD
`, 1),
			target: ErrDuplicateSource,
		},
		{
			name:   "invalid secretRef",
			yaml:   strings.Replace(valid, "env:EXPENSES_DB_PASSWORD", "actual-password", 1),
			target: ErrInvalidSecretRef,
		},
		{
			name:   "manual route without approver",
			yaml:   strings.Replace(valid, "    approver: bob\n", "", 1),
			target: ErrInvalidApprovalRoute,
		},
		{
			name:   "automatic approval disabled",
			yaml:   strings.Replace(valid, "    mode: manual", "    mode: auto", 1),
			target: ErrInvalidApprovalRoute,
		},
		{
			name:   "route with missing budget",
			yaml:   strings.Replace(valid, "budget_profile: detail_manual", "budget_profile: absent", 1),
			target: ErrInvalidApprovalRoute,
		},
		{
			name:   "invalid budget",
			yaml:   strings.Replace(valid, "    query_timeout: 5s", "    query_timeout: 45s", 1),
			target: ErrInvalidBudgetProfile,
		},
		{
			name: "V1 exposure profile removed",
			yaml: strings.Replace(valid, "    task_ttl: 30m", `    task_ttl: 30m
    max_release_facts: 10
    max_influence_facts: 20
    exposure_profile_version: taskgate-exposure-v1`, 1),
			target: ErrInvalidBudgetProfile,
		},
		{
			name:   "unattested type modifier",
			yaml:   strings.Replace(valid, "type: numeric", "type: numeric(10,2)", 1),
			target: ErrInvalidCatalog,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.yaml))
			if !errors.Is(err, test.target) {
				t.Fatalf("Parse error = %v, want %v", err, test.target)
			}
			if strings.Contains(err.Error(), "do-not-log-this") {
				t.Fatalf("error leaked plaintext password: %v", err)
			}
		})
	}
}

func TestCatalogRejectsUnknownFieldsAndNumericDurations(t *testing.T) {
	valid := validCatalogYAML(t)
	withUnknown := strings.Replace(valid, "catalog_version: \"2026.07.21\"", "catalog_version: \"2026.07.21\"\nunknown_key: value", 1)
	if _, err := Parse([]byte(withUnknown)); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("unknown field error = %v", err)
	}
	withNumericDuration := strings.Replace(valid, "max_db_time: 30s", "max_db_time: 30", 1)
	if _, err := Parse([]byte(withNumericDuration)); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("numeric duration error = %v", err)
	}
}

func TestCatalogRejectsEmbeddedPassword(t *testing.T) {
	valid := validCatalogYAML(t)
	withPassword := strings.Replace(valid, "address: postgres", "address: postgres://reader:super-secret@postgres/db", 1)
	_, err := Parse([]byte(withPassword))
	if !errors.Is(err, ErrPlaintextPassword) {
		t.Fatalf("embedded password error = %v", err)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked embedded password: %v", err)
	}
}

// The validator admits exact AVG on V2 products; the shipped Catalog keeps its
// aggregate lists unchanged because the private Dataset Binding pins the file's
// digest, so admitting AVG there is a re-binding decision, not a code change.
func TestV2ProductMayApproveAvgWhileUnknownAggregatesFailClosed(t *testing.T) {
	data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatalf("read repository catalog: %v", err)
	}
	original := "allowed_aggregates: [sum, count, min, max]"
	if !bytes.Contains(data, []byte(original)) {
		t.Fatalf("repository catalog no longer carries %q", original)
	}
	withAvg := bytes.Replace(data, []byte(original), []byte("allowed_aggregates: [sum, count, min, max, avg]"), 1)
	parsed, err := Parse(withAvg)
	if err != nil {
		t.Fatalf("Parse with avg returned error: %v", err)
	}
	approved := false
	for _, product := range parsed.Products {
		for _, aggregate := range product.AllowedAggregates {
			approved = approved || aggregate == "avg"
		}
	}
	if !approved {
		t.Fatal("parsed catalog does not expose the approved avg aggregate")
	}
	withMedian := bytes.Replace(data, []byte(original), []byte("allowed_aggregates: [sum, count, median]"), 1)
	if _, err := Parse(withMedian); !errors.Is(err, ErrInvalidCatalog) || !strings.Contains(err.Error(), "V2 supports only") {
		t.Fatalf("unknown aggregate error = %v", err)
	}
}
