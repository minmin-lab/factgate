package kbformal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/principalattr"
)

// Budget is one route budget of the formal round. The values are the budgets
// the pilot runners gave their tasks directly (kb-pilot for E-KB1, E-KB2(b)
// and E-KB5; the larger E-KB3 limits for E-KB3 and E-KB4), so a deployed task
// is bounded exactly as its pilot counterpart was. The predicate-footprint
// limits are the ones the pilot harness attached to every V5 task.
type Budget struct {
	Name         string
	MaxQueries   int64
	MaxRows      int64
	MaxDBTime    string
	QueryTimeout string
	TaskTTL      string
	Release      int64
	Influence    int64
	Outcome      int64
}

var (
	// PilotBudget is kb-pilot (B_Q 256, B_R 800, B_D 2400, B_O 64).
	PilotBudget = Budget{Name: "kb-pilot-v1", MaxQueries: 256, MaxRows: 1_000_000, MaxDBTime: "2h", QueryTimeout: "20s",
		TaskTTL: "30m", Release: 800, Influence: 2400, Outcome: 64}
	// LargeBudget is the non-binding budget of the overhead and quality runs.
	LargeBudget = Budget{Name: "kb-large-v1", MaxQueries: 100_000, MaxRows: 10_000_000, MaxDBTime: "24h", QueryTimeout: "60s",
		TaskTTL: "12h", Release: 100_000_000, Influence: 1_000_000_000, Outcome: 10_000_000}
)

// Profile is one Catalog-bound deployment of the formal round.
type Profile struct {
	Name        string
	Epoch       string
	Budget      Budget
	Experiments []string
	// EligibilitySubjects adds the 60 E-KB2(a) subjects to the attribute
	// snapshot (and to the deployment's principals file).
	EligibilitySubjects bool
}

// Profiles are the four profiles of the protocol (正式轮规程 §3) and the three
// of the keyword round; kbformal.Rounds says which round deploys which.
var Profiles = []Profile{
	{Name: "kb-v2-pilot", Epoch: "policy-kb-2026-10-bge-v2", Budget: PilotBudget,
		Experiments: []string{"ekb1", "ekb2a", "ekb2b", "ekb5-document"}, EligibilitySubjects: true},
	{Name: "kb-v2-large", Epoch: "policy-kb-2026-10-bge-v2", Budget: LargeBudget, Experiments: []string{"ekb3-48k", "ekb4"}},
	{Name: "kb-20k-large", Epoch: "policy-kb-2026-10-bge-20k", Budget: LargeBudget, Experiments: []string{"ekb3-96k"}},
	{Name: "kb-v2c-pilot", Epoch: "policy-kb-2026-10-bge-v2c", Budget: PilotBudget, Experiments: []string{"ekb5-collection"}},
	// The keyword round (docs/kb_experiment_design.md, 关键词检索实验):
	// publications that declare keyword search, same corpora and chunks.
	{Name: "kb-v2kw-pilot", Epoch: "policy-kb-2026-10-bge-v2kw", Budget: PilotBudget, Experiments: []string{"ekb7"}},
	{Name: "kb-v2kw-large", Epoch: "policy-kb-2026-10-bge-v2kw", Budget: LargeBudget, Experiments: []string{"ekb6", "ekb8-48k"}},
	{Name: "kb-20kkw-large", Epoch: "policy-kb-2026-10-bge-20kkw", Budget: LargeBudget, Experiments: []string{"ekb8-96k"}},
}

// LookupProfile returns the profile with the given name.
func LookupProfile(name string) (Profile, bool) {
	for _, profile := range Profiles {
		if profile.Name == name {
			return profile, true
		}
	}
	return Profile{}, false
}

// The envelope kb-publish emits, and the pre-registered E-KB2 rule that
// replaces it: the same department and clearance mapping with the
// finance_controller widening (docs/kb_experiment_design.md, E-KB2).
const (
	publishedEligibility = "    eligibility:\n      scope_envelope:\n        kb_department:\n          from_attribute: department\n        classification:\n          from_attribute: clearance\n"
	formalEligibility    = "    eligibility:\n      scope_envelope:\n        kb_department:\n          from_attribute: department\n          widen_if:\n            - {attribute: job_family, any_of: [finance_controller]}\n        classification:\n          from_attribute: clearance\n"
)

// PublicationDir is where the frozen outputs of an epoch live in the repository.
func PublicationDir(repoRoot, epoch string) string {
	return filepath.Join(repoRoot, "evaluation", "kb-formal", "publications", epoch)
}

// ProfileCatalog builds the KB-only Catalog of a profile from the demo
// Catalog's source and the epoch's frozen fragment: the demo products,
// publications, scopes, routes and budgets are dropped; one route for the
// knowledge product's sensitivity points at the profile's budget; the
// fragment's eligibility block is replaced by the pre-registered rule; and the
// source carries the attested schema digest of the knowledge-product business
// database (an empty digest keeps the demo value and marks a bootstrap
// Catalog that a deployment's attestation will refuse).
func ProfileCatalog(repoRoot string, profile Profile, schemaDigest string) ([]byte, error) {
	demo, err := os.ReadFile(filepath.Join(repoRoot, "config", "catalog.yaml"))
	if err != nil {
		return nil, err
	}
	fragment, err := os.ReadFile(filepath.Join(PublicationDir(repoRoot, profile.Epoch), "catalog-fragment.yaml"))
	if err != nil {
		return nil, err
	}
	text := string(fragment)
	if strings.Count(text, publishedEligibility) != 1 {
		return nil, fmt.Errorf("%s: fragment does not carry the published eligibility block exactly once", profile.Epoch)
	}
	text = strings.Replace(text, publishedEligibility, formalEligibility, 1)
	var sensitivity struct {
		KnowledgeProducts []struct {
			Sensitivity string `yaml:"sensitivity"`
		} `yaml:"knowledge_products"`
	}
	if err := yaml.Unmarshal([]byte(text), &sensitivity); err != nil || len(sensitivity.KnowledgeProducts) != 1 {
		return nil, fmt.Errorf("%s: fragment must declare exactly one knowledge product", profile.Epoch)
	}
	var document map[string]any
	if err := yaml.Unmarshal(demo, &document); err != nil {
		return nil, fmt.Errorf("decode demo catalog: %w", err)
	}
	sources, ok := document["sources"].([]any)
	if !ok || len(sources) != 1 {
		return nil, errors.New("demo catalog must declare exactly one source")
	}
	if schemaDigest != "" {
		source, _ := sources[0].(map[string]any)
		source["schema_digest"] = schemaDigest
	}
	budget := profile.Budget
	base := map[string]any{
		"catalog_version":       "kb-formal-" + profile.Name + "-v1",
		"sources":               sources,
		"scopes":                []any{},
		"snapshot_publications": []any{},
		"products":              []any{},
		"approval_routes": []any{map[string]any{
			"sensitivity": sensitivity.KnowledgeProducts[0].Sensitivity, "mode": "manual", "approver": "bob", "budget_profile": budget.Name,
		}},
		"budget_profiles": []any{map[string]any{
			"name": budget.Name, "max_queries": budget.MaxQueries, "max_rows": budget.MaxRows, "max_db_time": budget.MaxDBTime,
			"query_timeout": budget.QueryTimeout, "task_ttl": budget.TaskTTL, "max_release_facts": budget.Release,
			"max_influence_facts": budget.Influence, "max_outcome_facts": budget.Outcome,
			"exposure_profile_version": "taskgate-exposure-v5",
			"predicate_footprint": map[string]any{"version": "taskgate-predicate-footprint-v1", "max_raw_literals_per_query": 1000,
				"max_unique_atoms_per_query": 9, "max_atom_payload_bytes": 4096, "max_total_atom_payload_bytes": 36864},
		}},
	}
	encoded, err := yaml.Marshal(base)
	if err != nil {
		return nil, err
	}
	merged, err := catalog.MergeYAMLFragment(encoded, []byte(text))
	if err != nil {
		return nil, fmt.Errorf("%s: merge fragment: %w", profile.Name, err)
	}
	if _, err := catalog.Parse(merged); err != nil {
		return nil, fmt.Errorf("%s: generated catalog does not validate: %w", profile.Name, err)
	}
	return merged, nil
}

// Subject is one principal of an attribute snapshot.
type Subject struct {
	Name       string
	Department string
	Role       string
	Families   []string
	Clearance  []string
}

// EligibilitySubjects are the 60 pre-registered E-KB2(a) subjects: 12
// departments x 5 roles, clearance by role, and finance-director as the only
// finance_controller (the pilot runner's construction, kept verbatim).
func EligibilitySubjects() []Subject {
	departments := []string{"engineering", "finance", "hr", "it", "legal", "marketing", "operations", "procurement", "product", "research", "sales", "support"}
	roles := []string{"staff", "hr", "manager", "director", "executive"}
	clearanceOf := func(role string) []string {
		switch role {
		case "manager":
			return []string{"public", "internal", "confidential"}
		case "director", "executive":
			return []string{"public", "internal", "confidential", "leadership"}
		default:
			return []string{"public", "internal"}
		}
	}
	subjects := make([]Subject, 0, len(departments)*len(roles))
	for _, department := range departments {
		for _, role := range roles {
			families := []string{role}
			if department == "finance" && role == "director" {
				families = append(families, "finance_controller")
			}
			subjects = append(subjects, Subject{Name: department + "-" + role, Department: department, Role: role,
				Families: families, Clearance: clearanceOf(role)})
		}
	}
	return subjects
}

// Alice is the requester of every task except E-KB2(a)'s: a finance
// controller with full clearance, so she may request any single department
// and classification while each task's grant stays as narrow as its
// experiment requires (正式轮规程 §4).
var Alice = Subject{Name: "alice", Department: "finance", Role: "director",
	Families: []string{"director", "finance_controller"}, Clearance: []string{"public", "internal", "confidential", "leadership"}}

// PrincipalAttributes renders the attribute snapshot of a profile.
func PrincipalAttributes(profile Profile) []byte {
	subjects := []Subject{Alice}
	if profile.EligibilitySubjects {
		subjects = append(subjects, EligibilitySubjects()...)
	}
	var out strings.Builder
	out.WriteString("version: taskgate-principal-attributes-v1\nsubjects:\n")
	for _, subject := range subjects {
		fmt.Fprintf(&out, "  - subject: %s\n    attributes:\n      department: [%s]\n      job_family: [%s]\n      clearance: [%s]\n",
			subject.Name, subject.Department, strings.Join(subject.Families, ", "), strings.Join(subject.Clearance, ", "))
	}
	return []byte(out.String())
}

// Attestations maps a profile name to the attested schema digest of its
// knowledge-product business database.
type Attestations struct {
	Version  string            `json:"version"`
	Note     string            `json:"note"`
	Profiles map[string]string `json:"profiles"`
}

// AttestationsVersion names the attestation file layout.
const AttestationsVersion = "taskgate-kb-formal-schema-attestations-v1"

// LoadAttestations reads profiles/schema-attestations.json, or returns an
// empty set when it does not exist yet (bootstrap).
func LoadAttestations(repoRoot string) (Attestations, error) {
	path := filepath.Join(ProfilesDir(repoRoot), "schema-attestations.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Attestations{Version: AttestationsVersion, Profiles: map[string]string{}}, nil
	}
	if err != nil {
		return Attestations{}, err
	}
	var attestations Attestations
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&attestations); err != nil {
		return Attestations{}, fmt.Errorf("decode schema attestations: %w", err)
	}
	if attestations.Version != AttestationsVersion {
		return Attestations{}, fmt.Errorf("schema attestations version %q is not %s", attestations.Version, AttestationsVersion)
	}
	if attestations.Profiles == nil {
		attestations.Profiles = map[string]string{}
	}
	return attestations, nil
}

// ProfilesDir is where generated profile files live.
func ProfilesDir(repoRoot string) string {
	return filepath.Join(repoRoot, "evaluation", "kb-formal", "profiles")
}

// RegistryEntry describes one generated profile.
type RegistryEntry struct {
	Name                 string   `json:"name"`
	Epoch                string   `json:"epoch"`
	Budget               string   `json:"budget_profile"`
	Experiments          []string `json:"experiments"`
	Catalog              string   `json:"catalog"`
	CatalogSHA256        string   `json:"catalog_sha256"`
	Attributes           string   `json:"principal_attributes"`
	AttributesSHA256     string   `json:"principal_attributes_sha256"`
	KBPublicationSHA256  string   `json:"kb_publication_sha256"`
	SchemaDigest         string   `json:"schema_digest,omitempty"`
	SchemaDigestAttested bool     `json:"schema_digest_attested"`
	EligibilitySubjects  int      `json:"eligibility_subjects"`
}

// Registry lists every generated profile; its sha256 is what the formal
// gateway build seals as the profile registry.
type Registry struct {
	Version  string          `json:"version"`
	Profiles []RegistryEntry `json:"profiles"`
}

// RegistryVersion names the registry layout.
const RegistryVersion = "taskgate-kb-formal-profile-registry-v1"

// GeneratedFile is one file the generator writes.
type GeneratedFile struct {
	Path string
	Data []byte
}

// Generate renders every profile Catalog, attribute snapshot and the registry.
func Generate(repoRoot string) ([]GeneratedFile, error) {
	attestations, err := LoadAttestations(repoRoot)
	if err != nil {
		return nil, err
	}
	dir := ProfilesDir(repoRoot)
	registry := Registry{Version: RegistryVersion}
	var files []GeneratedFile
	for _, profile := range Profiles {
		digest := attestations.Profiles[profile.Name]
		catalogBytes, err := ProfileCatalog(repoRoot, profile, digest)
		if err != nil {
			return nil, err
		}
		attributes := PrincipalAttributes(profile)
		if _, err := principalattr.Parse(attributes); err != nil {
			return nil, fmt.Errorf("%s: attribute snapshot does not parse: %w", profile.Name, err)
		}
		manifest, err := os.ReadFile(filepath.Join(PublicationDir(repoRoot, profile.Epoch), "kb-publication.json"))
		if err != nil {
			return nil, err
		}
		catalogName, attributesName := profile.Name+".catalog.yaml", profile.Name+".principal-attributes.yaml"
		files = append(files, GeneratedFile{filepath.Join(dir, catalogName), catalogBytes}, GeneratedFile{filepath.Join(dir, attributesName), attributes})
		subjects := 0
		if profile.EligibilitySubjects {
			subjects = len(EligibilitySubjects())
		}
		registry.Profiles = append(registry.Profiles, RegistryEntry{Name: profile.Name, Epoch: profile.Epoch, Budget: profile.Budget.Name,
			Experiments: append([]string(nil), profile.Experiments...), Catalog: catalogName, CatalogSHA256: sum(catalogBytes),
			Attributes: attributesName, AttributesSHA256: sum(attributes), KBPublicationSHA256: sum(manifest),
			SchemaDigest: digest, SchemaDigestAttested: digest != "", EligibilitySubjects: subjects})
	}
	sort.Slice(registry.Profiles, func(i, j int) bool { return registry.Profiles[i].Name < registry.Profiles[j].Name })
	encoded, err := json.MarshalIndent(registry, "", " ")
	if err != nil {
		return nil, err
	}
	files = append(files, GeneratedFile{filepath.Join(dir, "registry.json"), append(encoded, '\n')})
	return files, nil
}

func sum(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
