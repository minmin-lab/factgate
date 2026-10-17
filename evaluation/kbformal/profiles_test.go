package kbformal

import (
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/principalattr"
)

func TestProfilesCarryTheirPreRegisteredSettings(t *testing.T) {
	root := filepath.Join("..", "..")
	files, err := Generate(root)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string][]byte{}
	for _, file := range files {
		byName[filepath.Base(file.Path)] = file.Data
	}
	for _, profile := range Profiles {
		parsed, err := catalog.Parse(byName[profile.Name+".catalog.yaml"])
		if err != nil {
			t.Fatalf("%s: %v", profile.Name, err)
		}
		if len(parsed.KnowledgeProducts) != 1 || parsed.KnowledgeProducts[0].IndexPublication != profile.Epoch {
			t.Fatalf("%s: catalog does not bind exactly epoch %s", profile.Name, profile.Epoch)
		}
		if len(parsed.Products) == 0 || len(parsed.ApprovalRoutes) != 1 || len(parsed.BudgetProfiles) != 1 {
			t.Fatalf("%s: want KB products only, one route and one budget profile", profile.Name)
		}
		budget := parsed.BudgetProfiles[0]
		want := profile.Budget
		if budget.Name != want.Name || budget.MaxQueries != want.MaxQueries || budget.MaxReleaseFacts != want.Release ||
			budget.MaxInfluenceFacts != want.Influence || budget.MaxOutcomeFacts != want.Outcome || budget.PredicateFootprint == nil ||
			budget.PredicateFootprint.MaxUniqueAtomsPerQuery != 9 || budget.ExposureProfileVersion != "taskgate-exposure-v5" {
			t.Fatalf("%s: budget profile %+v does not match %+v", profile.Name, budget, want)
		}
		if route := parsed.ApprovalRoutes[0]; route.BudgetProfile != want.Name || route.Approver != "bob" || route.Mode != "manual" {
			t.Fatalf("%s: route %+v", profile.Name, route)
		}
		text := string(byName[profile.Name+".catalog.yaml"])
		if !strings.Contains(text, "finance_controller") {
			t.Fatalf("%s: the pre-registered finance_controller widening is missing", profile.Name)
		}
		snapshot, err := principalattr.Parse(byName[profile.Name+".principal-attributes.yaml"])
		if err != nil {
			t.Fatalf("%s: %v", profile.Name, err)
		}
		_ = snapshot
		attributes := string(byName[profile.Name+".principal-attributes.yaml"])
		subjects := strings.Count(attributes, "  - subject: ")
		if want := 1; profile.EligibilitySubjects {
			if subjects != 61 {
				t.Fatalf("%s: %d subjects, want alice plus the 60 E-KB2(a) subjects", profile.Name, subjects)
			}
		} else if subjects != want {
			t.Fatalf("%s: %d subjects, want alice only", profile.Name, subjects)
		}
		if !strings.Contains(attributes, "  - subject: alice\n    attributes:\n      department: [finance]\n      job_family: [director, finance_controller]\n      clearance: [public, internal, confidential, leadership]\n") {
			t.Fatalf("%s: alice's attributes are not the protocol's", profile.Name)
		}
	}
	if !strings.Contains(string(byName["kb-v2-pilot.principal-attributes.yaml"]), "  - subject: finance-director\n    attributes:\n      department: [finance]\n      job_family: [director, finance_controller]\n") {
		t.Fatal("finance-director is not the only E-KB2(a) controller as pre-registered")
	}
	if strings.Count(string(byName["kb-v2-pilot.principal-attributes.yaml"]), "finance_controller") != 2 {
		t.Fatal("exactly alice and finance-director carry finance_controller")
	}
}
