// Package kbcorpus generates the frozen synthetic document corpus of the
// knowledge-product experiments (KB-1, docs/kb-design.md). Generation is a
// pure function of Spec: the committed manifest binds the seed and the
// SHA-256 of the JSONL it produces, so the corpus can be regenerated anywhere
// and checked byte for byte without storing 10,000 documents in git.
//
// The package imports no Gateway code. It knows nothing about chunks: the
// segmentation function belongs to the publisher (cmd/kb-publish).
package kbcorpus

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

const (
	Version   = "taskgate-kb-corpus-v1"
	Generator = "evaluation/cmd/kb-corpus"

	// FrontMatterDelimiter opens and closes the extractable field block that
	// starts every document body. Publication-time extraction rules with
	// source "frontmatter" read exactly this block.
	FrontMatterDelimiter = "---"
	// SectionPrefix starts every section heading; the classification of the
	// section follows the heading in square brackets.
	SectionPrefix = "## "

	ClassificationPublic       = "public"
	ClassificationInternal     = "internal"
	ClassificationConfidential = "confidential"
	ClassificationLeadership   = "leadership"
)

// Departments, Roles, Classifications, Labels and DocumentTypes are the
// closed vocabularies of the corpus. Their order is part of the generator.
var (
	Departments = []string{"sales", "engineering", "finance", "hr", "legal", "marketing",
		"operations", "procurement", "product", "research", "support", "it"}
	Roles           = []string{"staff", "manager", "director", "executive", "hr"}
	Classifications = []string{ClassificationPublic, ClassificationInternal, ClassificationConfidential, ClassificationLeadership}
	Labels          = []string{"budget", "pricing", "vendor", "hiring", "compliance", "roadmap",
		"incident", "audit", "travel", "training", "merger", "payroll"}
	DocumentTypes = []string{"contract", "approval", "policy", "report"}
)

// FieldsByType lists the extractable front-matter fields of each document
// type with their exposure value types, in front-matter order.
var FieldsByType = map[string][]Field{
	"contract": {{"contract_no", "text"}, {"amount", "numeric"}, {"counterparty", "text"}, {"signed_on", "date"}},
	"approval": {{"approval_no", "text"}, {"approved_amount", "numeric"}, {"applicant_no", "text"}, {"approved_on", "date"}},
	"policy":   {{"policy_no", "text"}, {"effective_on", "date"}},
	"report":   {{"period", "text"}, {"headline_metric", "numeric"}},
}

type Field struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Spec struct {
	Seed      uint64 `json:"seed"`
	Documents int    `json:"documents"`
}

type Section struct {
	Heading        string `json:"heading"`
	Classification string `json:"classification"`
	Text           string `json:"text"`
}

// Document is one document version. Body is the authoritative text: the
// front-matter block followed by the sections. Fields repeats the front
// matter for convenience and tests assert the two agree.
type Document struct {
	DocID          string            `json:"doc_id"`
	Version        int               `json:"version"`
	Title          string            `json:"title"`
	DocumentType   string            `json:"document_type"`
	Department     string            `json:"department"`
	Classification string            `json:"classification"`
	JobRole        string            `json:"job_role"`
	Labels         []string          `json:"labels"`
	ValidFrom      string            `json:"valid_from"`
	ValidUntil     string            `json:"valid_until,omitempty"`
	Fields         map[string]string `json:"fields"`
	Sections       []Section         `json:"sections"`
	Body           string            `json:"body"`
}

type Counts struct {
	Documents          int            `json:"documents"`
	Versions           int            `json:"versions"`
	Sections           int            `json:"sections"`
	LeadershipSections int            `json:"leadership_sections"`
	ByType             map[string]int `json:"by_type"`
	ByDepartment       map[string]int `json:"by_department"`
	ByClassification   map[string]int `json:"by_classification"`
	ByRole             map[string]int `json:"by_role"`
}

type Manifest struct {
	Version         string `json:"version"`
	Generator       string `json:"generator"`
	Spec            Spec   `json:"spec"`
	SpecSHA256      string `json:"spec_sha256"`
	Counts          Counts `json:"counts"`
	DocumentsBytes  int64  `json:"documents_bytes"`
	DocumentsSHA256 string `json:"documents_sha256"`
}

//go:embed manifest-v1.json
var frozenManifestBytes []byte

// FrozenManifest returns the committed digest binding of corpus v1.
func FrozenManifest() (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(frozenManifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode frozen kb corpus manifest: %w", err)
	}
	return manifest, nil
}

type generator struct {
	rng  *rand.Rand
	spec Spec
}

// Generate produces every document version of the corpus in a deterministic
// order (by doc_id, then version).
func Generate(spec Spec) ([]Document, error) {
	if spec.Documents <= 0 || spec.Documents > 1_000_000 {
		return nil, errors.New("kb corpus: documents must be between 1 and 1000000")
	}
	g := &generator{rng: rand.New(rand.NewPCG(spec.Seed, spec.Seed^0x9e3779b97f4a7c15)), spec: spec}
	documents := make([]Document, 0, spec.Documents+spec.Documents/20+1)
	for index := 0; index < spec.Documents; index++ {
		first := g.document(index, 1, nil)
		documents = append(documents, first)
		if g.rng.IntN(20) == 0 {
			documents = append(documents, g.document(index, 2, &first))
		}
	}
	return documents, nil
}

func (g *generator) pick(values []string) string { return values[g.rng.IntN(len(values))] }

func (g *generator) document(index, version int, previous *Document) Document {
	var doc Document
	if previous == nil {
		doc = Document{
			DocID: fmt.Sprintf("D-%06d", index+1), Version: 1,
			DocumentType: g.pick(DocumentTypes), Department: g.pick(Departments),
		}
		// Document-level classification: 5% leadership, 25% confidential,
		// 50% internal, 20% public.
		switch roll := g.rng.IntN(100); {
		case roll < 5:
			doc.Classification = ClassificationLeadership
		case roll < 30:
			doc.Classification = ClassificationConfidential
		case roll < 80:
			doc.Classification = ClassificationInternal
		default:
			doc.Classification = ClassificationPublic
		}
		doc.JobRole = g.roleFor(doc.Classification)
		doc.Labels = g.labels()
		validFrom := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, g.rng.IntN(900))
		doc.ValidFrom = validFrom.Format("2006-01-02")
		if g.rng.IntN(3) == 0 {
			doc.ValidUntil = validFrom.AddDate(0, 0, 90+g.rng.IntN(720)).Format("2006-01-02")
		}
		doc.Fields = g.fields(doc, validFrom)
	} else {
		doc = *previous
		doc.Version = version
		doc.Labels = append([]string(nil), previous.Labels...)
		doc.Fields = make(map[string]string, len(previous.Fields))
		for name, value := range previous.Fields {
			doc.Fields[name] = value
		}
		// A new version revises one numeric or date field when the type has one.
		for _, field := range FieldsByType[doc.DocumentType] {
			if field.Type == "numeric" {
				doc.Fields[field.Name] = g.amount()
				break
			}
		}
	}
	doc.Title = g.title(doc)
	doc.Sections = g.sections(doc, previous)
	doc.Body = RenderBody(doc)
	return doc
}

func (g *generator) roleFor(classification string) string {
	switch classification {
	case ClassificationLeadership:
		return "executive"
	case ClassificationConfidential:
		return []string{"manager", "director", "hr"}[g.rng.IntN(3)]
	case ClassificationInternal:
		return []string{"staff", "manager"}[g.rng.IntN(2)]
	default:
		return "staff"
	}
}

func (g *generator) labels() []string {
	count := g.rng.IntN(4)
	chosen := map[string]struct{}{}
	for len(chosen) < count {
		chosen[g.pick(Labels)] = struct{}{}
	}
	labels := make([]string, 0, len(chosen))
	for label := range chosen {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

func (g *generator) amount() string {
	return fmt.Sprintf("%d.%02d", 1000+g.rng.IntN(2_000_000), g.rng.IntN(100))
}

var counterparties = []string{"Acme Logistics", "Borealis Systems", "Cedar Analytics", "Delta Freight",
	"Evergreen Media", "Fulcrum Capital", "Granite Works", "Helios Energy", "Ionic Labs", "Juniper Health"}

func (g *generator) fields(doc Document, validFrom time.Time) map[string]string {
	fields := map[string]string{}
	serial := g.rng.IntN(900000) + 100000
	switch doc.DocumentType {
	case "contract":
		fields["contract_no"] = fmt.Sprintf("C-%d", serial)
		fields["amount"] = g.amount()
		fields["counterparty"] = g.pick(counterparties)
		fields["signed_on"] = validFrom.AddDate(0, 0, -g.rng.IntN(30)).Format("2006-01-02")
	case "approval":
		fields["approval_no"] = fmt.Sprintf("A-%d", serial)
		fields["approved_amount"] = g.amount()
		fields["applicant_no"] = fmt.Sprintf("E%04d", g.rng.IntN(9000)+1000)
		fields["approved_on"] = validFrom.Format("2006-01-02")
	case "policy":
		fields["policy_no"] = fmt.Sprintf("P-%d", serial)
		fields["effective_on"] = validFrom.Format("2006-01-02")
	case "report":
		fields["period"] = fmt.Sprintf("%d-Q%d", validFrom.Year(), (int(validFrom.Month())-1)/3+1)
		fields["headline_metric"] = g.amount()
	}
	return fields
}

func (g *generator) title(doc Document) string {
	topic := g.pick(topicsByDepartment[doc.Department])
	switch doc.DocumentType {
	case "contract":
		return fmt.Sprintf("%s contract with %s on %s", strings.Title(doc.Department), doc.Fields["counterparty"], topic)
	case "approval":
		return fmt.Sprintf("%s approval %s for %s", strings.Title(doc.Department), doc.Fields["approval_no"], topic)
	case "policy":
		return fmt.Sprintf("%s policy on %s", strings.Title(doc.Department), topic)
	default:
		return fmt.Sprintf("%s report %s: %s", strings.Title(doc.Department), doc.Fields["period"], topic)
	}
}

var topicsByDepartment = map[string][]string{
	"sales":       {"territory quotas", "discount approvals", "pipeline reviews", "channel partner terms", "renewal targets"},
	"engineering": {"release cadence", "incident postmortems", "on-call rotation", "dependency upgrades", "capacity planning"},
	"finance":     {"quarterly close", "expense reimbursement", "cash forecasting", "audit findings", "cost allocation"},
	"hr":          {"compensation bands", "hiring plans", "performance cycles", "leave policy", "payroll corrections"},
	"legal":       {"contract templates", "data processing terms", "litigation holds", "trademark filings", "export controls"},
	"marketing":   {"campaign budgets", "brand guidelines", "event sponsorships", "lead scoring", "pricing pages"},
	"operations":  {"facility leases", "fleet maintenance", "vendor onboarding", "safety inspections", "shift scheduling"},
	"procurement": {"supplier scorecards", "tender evaluations", "purchase thresholds", "framework agreements", "invoice matching"},
	"product":     {"roadmap priorities", "feature retirements", "customer councils", "pricing experiments", "usage metrics"},
	"research":    {"grant proposals", "lab safety", "publication reviews", "dataset licensing", "prototype budgets"},
	"support":     {"escalation paths", "service credits", "knowledge base curation", "response targets", "tooling renewals"},
	"it":          {"access reviews", "device refresh", "backup retention", "license audits", "network changes"},
}

var sentenceTemplates = []string{
	"The %s team reviewed %s during %s and recorded %s as the governing figure.",
	"Under this %s, %s applies to the %s department from %s onward.",
	"Approval of %s requires sign-off at the %s level and is tracked under reference %s.",
	"Exceptions to %s must be documented, and the %s owner reports them in the %s review.",
	"The %s baseline for %s is %s, reviewed each %s.",
	"Any change to %s in %s is logged with its reference %s before it takes effect.",
}

var periodWords = []string{"the first quarter", "the second quarter", "the third quarter", "the fourth quarter",
	"the annual review", "the mid-year checkpoint"}

func (g *generator) sentence(doc Document) string {
	topics := topicsByDepartment[doc.Department]
	reference := ""
	for _, field := range FieldsByType[doc.DocumentType] {
		if field.Type == "text" {
			reference = doc.Fields[field.Name]
			break
		}
	}
	figure := ""
	for _, field := range FieldsByType[doc.DocumentType] {
		if field.Type == "numeric" {
			figure = doc.Fields[field.Name]
			break
		}
	}
	if figure == "" {
		figure = doc.Fields["effective_on"]
	}
	args := [][]any{
		{doc.Department, g.pick(topics), g.pick(periodWords), figure},
		{doc.DocumentType, g.pick(topics), doc.Department, doc.ValidFrom},
		{g.pick(topics), g.pick(Roles), reference},
		{g.pick(topics), doc.Department, g.pick(periodWords)},
		{doc.Department, g.pick(topics), figure, g.pick(periodWords)},
		{g.pick(topics), doc.Department, reference},
	}
	index := g.rng.IntN(len(sentenceTemplates))
	return fmt.Sprintf(sentenceTemplates[index], args[index]...)
}

var sectionHeadings = []string{"Purpose", "Scope", "Terms", "Responsibilities", "Review", "Appendix"}

func (g *generator) sections(doc Document, previous *Document) []Section {
	if previous != nil {
		// A new version keeps the section plan and rewrites the last section.
		sections := append([]Section(nil), previous.Sections...)
		last := &sections[len(sections)-1]
		last.Text = g.paragraph(doc, 3+g.rng.IntN(3))
		return sections
	}
	count := 3 + g.rng.IntN(4)
	sections := make([]Section, 0, count+1)
	for index := 0; index < count; index++ {
		sections = append(sections, Section{
			Heading: sectionHeadings[index%len(sectionHeadings)], Classification: doc.Classification,
			Text: g.paragraph(doc, 3+g.rng.IntN(4)),
		})
	}
	// About 10% of non-leadership documents carry one leadership-only section.
	if doc.Classification != ClassificationLeadership && g.rng.IntN(10) == 0 {
		sections = append(sections, Section{
			Heading: "Leadership assessment", Classification: ClassificationLeadership,
			Text: "Leadership-only assessment. " + g.paragraph(doc, 2+g.rng.IntN(3)),
		})
	}
	return sections
}

func (g *generator) paragraph(doc Document, sentences int) string {
	parts := make([]string, 0, sentences)
	for index := 0; index < sentences; index++ {
		parts = append(parts, g.sentence(doc))
	}
	return strings.Join(parts, " ")
}

// RenderBody renders the authoritative text of a document version: the
// front-matter block in FieldsByType order, then every section.
func RenderBody(doc Document) string {
	var body strings.Builder
	body.WriteString(FrontMatterDelimiter + "\n")
	for _, field := range FieldsByType[doc.DocumentType] {
		body.WriteString(field.Name + ": " + doc.Fields[field.Name] + "\n")
	}
	body.WriteString(FrontMatterDelimiter + "\n")
	for _, section := range doc.Sections {
		body.WriteString(SectionPrefix + section.Heading + " [" + section.Classification + "]\n")
		body.WriteString(section.Text + "\n")
	}
	return body.String()
}

// ParseFrontMatter reads the extractable field block at the start of a body
// and returns the fields with the remaining text. It is the parser the
// publication-time "frontmatter" extraction source uses.
func ParseFrontMatter(body string) (map[string]string, string, error) {
	lines := strings.SplitAfter(body, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\n") != FrontMatterDelimiter {
		return nil, "", errors.New("front matter: missing opening delimiter")
	}
	fields := map[string]string{}
	consumed := len(lines[0])
	for _, line := range lines[1:] {
		consumed += len(line)
		trimmed := strings.TrimRight(line, "\n")
		if trimmed == FrontMatterDelimiter {
			return fields, body[consumed:], nil
		}
		name, value, ok := strings.Cut(trimmed, ": ")
		if !ok || name == "" || value == "" {
			return nil, "", fmt.Errorf("front matter: malformed line %q", trimmed)
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, "", fmt.Errorf("front matter: duplicate field %q", name)
		}
		fields[name] = value
	}
	return nil, "", errors.New("front matter: missing closing delimiter")
}

// ParseSections splits the text after the front matter into sections with
// their declared classification.
func ParseSections(rest string) ([]Section, error) {
	var sections []Section
	for _, block := range strings.Split(rest, SectionPrefix) {
		if strings.TrimSpace(block) == "" {
			continue
		}
		headingLine, text, _ := strings.Cut(block, "\n")
		open := strings.LastIndex(headingLine, " [")
		if open < 0 || !strings.HasSuffix(headingLine, "]") {
			return nil, fmt.Errorf("section heading %q lacks a classification", headingLine)
		}
		sections = append(sections, Section{
			Heading: headingLine[:open], Classification: headingLine[open+2 : len(headingLine)-1],
			Text: strings.TrimRight(text, "\n"),
		})
	}
	if len(sections) == 0 {
		return nil, errors.New("document has no sections")
	}
	return sections, nil
}

// EncodeJSONL renders one canonical JSON object per line.
func EncodeJSONL(documents []Document) ([]byte, error) {
	var out bytes.Buffer
	for _, doc := range documents {
		line, err := json.Marshal(doc)
		if err != nil {
			return nil, err
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// DecodeJSONL reads a corpus file produced by EncodeJSONL.
func DecodeJSONL(data []byte) ([]Document, error) {
	var documents []Document
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	for decoder.More() {
		var doc Document
		if err := decoder.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode kb corpus line %d: %w", len(documents)+1, err)
		}
		documents = append(documents, doc)
	}
	return documents, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// BuildManifest binds a spec to the JSONL it generated.
func BuildManifest(spec Spec, documents []Document, jsonl []byte) (Manifest, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return Manifest{}, err
	}
	counts := Counts{ByType: map[string]int{}, ByDepartment: map[string]int{}, ByClassification: map[string]int{}, ByRole: map[string]int{}}
	seen := map[string]struct{}{}
	for _, doc := range documents {
		seen[doc.DocID] = struct{}{}
		counts.Versions++
		counts.ByType[doc.DocumentType]++
		counts.ByDepartment[doc.Department]++
		counts.ByClassification[doc.Classification]++
		counts.ByRole[doc.JobRole]++
		for _, section := range doc.Sections {
			counts.Sections++
			if section.Classification == ClassificationLeadership {
				counts.LeadershipSections++
			}
		}
	}
	counts.Documents = len(seen)
	return Manifest{
		Version: Version, Generator: Generator, Spec: spec, SpecSHA256: sha256Hex(specJSON),
		Counts: counts, DocumentsBytes: int64(len(jsonl)), DocumentsSHA256: sha256Hex(jsonl),
	}, nil
}

// Build generates the corpus and its manifest for a spec.
func Build(spec Spec) ([]Document, []byte, Manifest, error) {
	documents, err := Generate(spec)
	if err != nil {
		return nil, nil, Manifest{}, err
	}
	jsonl, err := EncodeJSONL(documents)
	if err != nil {
		return nil, nil, Manifest{}, err
	}
	manifest, err := BuildManifest(spec, documents, jsonl)
	if err != nil {
		return nil, nil, Manifest{}, err
	}
	return documents, jsonl, manifest, nil
}

// EncodeManifest renders a manifest deterministically.
func EncodeManifest(manifest Manifest) ([]byte, error) {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
