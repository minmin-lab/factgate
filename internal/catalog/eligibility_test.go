package catalog

import (
	"errors"
	"strings"
	"testing"
)

const eligibilityBlock = `    eligibility:
      require:
        - attribute: job_family
          any_of: [hr, finance_controller]
      scope_envelope:
        department:
          from_attribute: department
          widen_if:
            - attribute: job_family
              any_of: [finance_controller]
`

// withEligibility attaches an eligibility declaration to expense_detail.
func withEligibility(t *testing.T, block string) string {
	t.Helper()
	valid := validCatalogYAML(t)
	// The top-level scopes section immediately follows expense_detail.
	anchor := "\nscopes:\n"
	if strings.Count(valid, anchor) != 1 {
		t.Fatalf("fixture anchor not unique")
	}
	return strings.Replace(valid, anchor, "\n"+block+"scopes:\n", 1)
}

func TestEligibilityParsesStrictly(t *testing.T) {
	parsed, err := Parse([]byte(withEligibility(t, eligibilityBlock)))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	var detail *Product
	for i := range parsed.Products {
		if parsed.Products[i].Name == "expense_detail" {
			detail = &parsed.Products[i]
		}
	}
	if detail == nil || detail.Eligibility == nil {
		t.Fatalf("expense_detail eligibility missing: %#v", parsed.Products)
	}
	if len(detail.Eligibility.Require) != 1 || detail.Eligibility.Require[0].Attribute != "job_family" ||
		strings.Join(detail.Eligibility.Require[0].AnyOf, ",") != "hr,finance_controller" {
		t.Fatalf("unexpected require: %#v", detail.Eligibility.Require)
	}
	rule, ok := detail.Eligibility.ScopeEnvelope["department"]
	if !ok || rule.FromAttribute != "department" || len(rule.WidenIf) != 1 || rule.WidenIf[0].AnyOf[0] != "finance_controller" {
		t.Fatalf("unexpected scope envelope: %#v", detail.Eligibility.ScopeEnvelope)
	}
	if parsed.Products[0].Eligibility != nil {
		t.Fatalf("expense_summary unexpectedly carries eligibility")
	}
	// A product with eligibility must hash differently from one without.
	plain, err := Parse([]byte(validCatalogYAML(t)))
	if err != nil {
		t.Fatal(err)
	}
	if plain.SHA256 == parsed.SHA256 {
		t.Fatal("eligibility declaration did not change the catalog digest")
	}
}

func TestEligibilityRejectsInvalidDeclarations(t *testing.T) {
	tests := []struct {
		name    string
		block   string
		message string
	}{
		{
			name:    "empty declaration",
			block:   "    eligibility: {}\n",
			message: "eligibility must declare require conditions or a scope envelope",
		},
		{
			name: "uppercase attribute name",
			block: `    eligibility:
      require:
        - attribute: JobFamily
          any_of: [hr]
`,
			message: "products[1].eligibility.require[0].attribute",
		},
		{
			name: "empty any_of",
			block: `    eligibility:
      require:
        - attribute: job_family
          any_of: []
`,
			message: "any_of must list distinct non-empty values",
		},
		{
			name: "duplicate any_of",
			block: `    eligibility:
      require:
        - attribute: job_family
          any_of: [hr, hr]
`,
			message: "any_of must list distinct non-empty values",
		},
		{
			name: "non-canonical value",
			block: `    eligibility:
      require:
        - attribute: job_family
          any_of: [" hr"]
`,
			message: "attribute values must be canonical tokens",
		},
		{
			name: "envelope on scope not bound to product",
			block: `    eligibility:
      scope_envelope:
        expense_date:
          from_attribute: department
`,
			message: "scope envelope must name a mandatory scope of this product",
		},
		{
			name: "envelope on unknown scope",
			block: `    eligibility:
      scope_envelope:
        region:
          from_attribute: region
`,
			message: "scope envelope must name a mandatory scope of this product",
		},
		{
			name: "envelope with invalid from_attribute",
			block: `    eligibility:
      scope_envelope:
        department:
          from_attribute: "Department"
`,
			message: "products[1].eligibility.scope_envelope.department.from_attribute",
		},
		{
			name: "invalid widen_if condition",
			block: `    eligibility:
      scope_envelope:
        department:
          from_attribute: department
          widen_if:
            - attribute: job_family
              any_of: []
`,
			message: "scope_envelope.department.widen_if[0].any_of",
		},
		{
			name: "unknown field is rejected by strict decoding",
			block: `    eligibility:
      require:
        - attribute: job_family
          any_of: [hr]
      allow_override: true
`,
			message: "allow_override",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(withEligibility(t, tc.block)))
			if err == nil {
				t.Fatalf("Parse accepted invalid eligibility")
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.message)
			}
			if tc.name != "unknown field is rejected by strict decoding" && !errors.Is(err, ErrInvalidCatalog) {
				t.Fatalf("error = %v, want ErrInvalidCatalog", err)
			}
		})
	}
}

func TestEligibilityEnvelopeRejectsNonEnumScope(t *testing.T) {
	// Bind expense_date to expense_detail, then try to derive an envelope from it.
	modified := strings.Replace(withEligibility(t, `    eligibility:
      scope_envelope:
        expense_date:
          from_attribute: department
`), "    entity_key: [claim_number]\n", "    entity_key: [claim_number]\n    scopes: [department, expense_date]\n", 1)
	// Remove the original single-scope line of expense_detail so the product binds both scopes.
	modified = strings.Replace(modified, "        description: Employee department\n    scopes: [department]\n", "        description: Employee department\n", 1)
	_, err := Parse([]byte(modified))
	if err == nil || !strings.Contains(err.Error(), "scope envelopes are defined for enum scopes only") {
		t.Fatalf("error = %v, want enum-only rejection", err)
	}
}
