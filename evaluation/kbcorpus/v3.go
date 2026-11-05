package kbcorpus

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// Corpus v3 is the demo stack's five-grade corpus. Its metadata plan is the
// unchanged v1 plan (Generate) followed by a deterministic remap: the v1
// confidential grade splits into secret and confidential by a hash of the
// doc_id, leadership becomes top_secret, section headings become Chinese, the
// document grade is the highest section grade and job_role is redrawn for it.
// Prose is written by the v2 generator, so the v3 artifact is frozen by digest
// like v2.

const (
	VersionV3   = "taskgate-kb-corpus-v3"
	GeneratorV3 = "evaluation/cmd/kb-corpus-v3"

	ClassificationSecret    = "secret"
	ClassificationTopSecret = "top_secret"

	// v3SecretPercent is the share of v1 confidential documents graded secret.
	v3SecretPercent = 60
)

// ClassificationsV3 lists the five grades from lowest to highest. The order
// is the grade order PlanV3 takes the maximum over.
var ClassificationsV3 = []string{ClassificationPublic, ClassificationInternal, ClassificationSecret,
	ClassificationConfidential, ClassificationTopSecret}

// sectionHeadingsZH maps every v1 section heading (sectionHeadings plus the
// leadership-only heading) to its v3 Chinese heading.
var sectionHeadingsZH = map[string]string{
	"Purpose": "目的", "Scope": "范围", "Terms": "条款", "Responsibilities": "职责", "Review": "审查", "Appendix": "附录",
	"Leadership assessment": "管理层评估",
}

// rolesByClassificationV3 is the v3 job_role draw table, keyed by the
// document grade.
var rolesByClassificationV3 = map[string][]string{
	ClassificationTopSecret:    {"executive"},
	ClassificationConfidential: {"manager", "director", "hr"},
	ClassificationSecret:       {"manager", "staff"},
	ClassificationInternal:     {"staff", "manager"},
	ClassificationPublic:       {"staff"},
}

// ClassificationRankV3 returns the position of a grade in ClassificationsV3,
// or -1 when the value is not a v3 grade.
func ClassificationRankV3(classification string) int {
	for rank, candidate := range ClassificationsV3 {
		if candidate == classification {
			return rank
		}
	}
	return -1
}

// v3Draw is the deterministic draw of corpus v3: the first 8 bytes (big
// endian) of sha256(seed in decimal || "|" || doc_id || suffix). Every version
// of a document shares the draw because it depends on the doc_id only.
func v3Draw(seed uint64, docID, suffix string) uint64 {
	sum := sha256.Sum256([]byte(strconv.FormatUint(seed, 10) + "|" + docID + suffix))
	return binary.BigEndian.Uint64(sum[:8])
}

// v3SectionClassification maps one v1 section grade to its v3 grade.
func v3SectionClassification(seed uint64, docID, classification string) (string, error) {
	switch classification {
	case ClassificationPublic, ClassificationInternal:
		return classification, nil
	case ClassificationConfidential:
		if v3Draw(seed, docID, "")%100 < v3SecretPercent {
			return ClassificationSecret, nil
		}
		return ClassificationConfidential, nil
	case ClassificationLeadership:
		return ClassificationTopSecret, nil
	default:
		return "", fmt.Errorf("kb corpus v3: %s has unknown v1 classification %q", docID, classification)
	}
}

// v3JobRole redraws job_role for the document grade. The draw uses its own
// hash domain so it is independent of the secret/confidential split.
func v3JobRole(seed uint64, docID, classification string) (string, error) {
	roles, ok := rolesByClassificationV3[classification]
	if !ok {
		return "", fmt.Errorf("kb corpus v3: %s has no job_role table for %q", docID, classification)
	}
	return roles[v3Draw(seed, docID, "|job_role")%uint64(len(roles))], nil
}

// PlanV3 returns the v3 metadata plan: the v1 plan of spec, remapped to five
// grades with Chinese section headings and re-rendered bodies. It fails
// closed on a heading or grade the remap does not know.
func PlanV3(spec Spec) ([]Document, error) {
	plans, err := Generate(spec)
	if err != nil {
		return nil, err
	}
	documents := make([]Document, 0, len(plans))
	for _, plan := range plans {
		doc := plan
		doc.Sections = make([]Section, len(plan.Sections))
		top := -1
		for index, section := range plan.Sections {
			heading, ok := sectionHeadingsZH[section.Heading]
			if !ok {
				return nil, fmt.Errorf("kb corpus v3: %s v%d section %d heading %q has no Chinese heading", plan.DocID, plan.Version, index, section.Heading)
			}
			classification, err := v3SectionClassification(spec.Seed, plan.DocID, section.Classification)
			if err != nil {
				return nil, err
			}
			doc.Sections[index] = Section{Heading: heading, Classification: classification, Text: section.Text}
			if rank := ClassificationRankV3(classification); rank > top {
				top = rank
				doc.Classification = classification
			}
		}
		if top < 0 {
			return nil, fmt.Errorf("kb corpus v3: %s v%d has no sections", plan.DocID, plan.Version)
		}
		if doc.JobRole, err = v3JobRole(spec.Seed, doc.DocID, doc.Classification); err != nil {
			return nil, err
		}
		doc.Body = RenderBody(doc)
		documents = append(documents, doc)
	}
	return documents, nil
}

// v3GradeNames are the grade names a v3 section's prose may contain only when
// the name is its own grade. 公开 and 内部 are everyday words and are not
// checked.
var v3GradeNames = []string{classificationNamesZH[ClassificationSecret], classificationNamesZH[ClassificationConfidential],
	classificationNamesZH[ClassificationTopSecret]}

// CheckGradeNamesV3 rejects a v3 document version whose section prose names
// a grade other than the section's own (prompt rule 8), for example 机密 in a
// secret section or any of 秘密/机密/绝密 in a public or internal section.
func CheckGradeNamesV3(doc Document) error {
	for index, section := range doc.Sections {
		own := classificationNamesZH[section.Classification]
		for _, name := range v3GradeNames {
			if name != own && strings.Contains(section.Text, name) {
				return fmt.Errorf("%s v%d: section %d [%s] names the grade %s", doc.DocID, doc.Version, index, section.Classification, name)
			}
		}
	}
	return nil
}

// ApplyGeneratedV3 is ApplyGenerated for the five-grade plan: the same plan
// validation, then CheckGradeNamesV3. The v2 path keeps calling
// ApplyGenerated and is unchanged.
func ApplyGeneratedV3(plan Document, generated GeneratedDocument, previous *Document) (Document, error) {
	doc, err := ApplyGenerated(plan, generated, previous)
	if err != nil {
		return Document{}, err
	}
	if err := CheckGradeNamesV3(doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}
