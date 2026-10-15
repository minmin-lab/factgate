// Package principalattr loads the attested subject-attribute snapshot that
// eligibility compilation consumes. The snapshot is an input supplied by the
// identity/HR authority; the gateway validates its shape and binds its digest
// into every eligibility verdict, and does not judge attribute correctness.
package principalattr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const Version = "taskgate-principal-attributes-v1"

var identifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
var subjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.@-]{0,127}$`)

// Grant is a temporary attribute value with an explicit validity window.
type Grant struct {
	Attribute  string `yaml:"attribute" json:"attribute"`
	Value      string `yaml:"value" json:"value"`
	ValidFrom  string `yaml:"valid_from" json:"valid_from"`
	ValidUntil string `yaml:"valid_until" json:"valid_until"`

	validFrom  time.Time
	validUntil time.Time
}

// Subject carries the standing attributes and temporary grants of one
// authenticated subject.
type Subject struct {
	Subject    string              `yaml:"subject" json:"subject"`
	Attributes map[string][]string `yaml:"attributes" json:"attributes"`
	Grants     []Grant             `yaml:"grants,omitempty" json:"grants,omitempty"`
}

// Snapshot is the loaded, validated attribute file with its content digest.
type Snapshot struct {
	Version  string    `yaml:"version" json:"version"`
	Subjects []Subject `yaml:"subjects" json:"subjects"`

	SHA256 string `yaml:"-" json:"-"`
	index  map[string]*Subject
}

// Load reads and validates a snapshot file.
func Load(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read principal attributes: %w", err)
	}
	return Parse(data)
}

// Parse validates a YAML snapshot and computes its canonical digest.
func Parse(data []byte) (*Snapshot, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	var snapshot Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("parse principal attributes: %w", err)
	}
	if err := snapshot.validate(); err != nil {
		return nil, err
	}
	digest, err := snapshot.digest()
	if err != nil {
		return nil, err
	}
	snapshot.SHA256 = digest
	return &snapshot, nil
}

func (s *Snapshot) validate() error {
	if s.Version != Version {
		return fmt.Errorf("principal attributes: version must be %q", Version)
	}
	if len(s.Subjects) == 0 {
		return errors.New("principal attributes: at least one subject is required")
	}
	s.index = make(map[string]*Subject, len(s.Subjects))
	for i := range s.Subjects {
		subject := &s.Subjects[i]
		if !subjectPattern.MatchString(subject.Subject) {
			return fmt.Errorf("principal attributes: subjects[%d].subject is not a canonical subject identifier", i)
		}
		if _, duplicate := s.index[subject.Subject]; duplicate {
			return fmt.Errorf("principal attributes: subject %q is duplicated", subject.Subject)
		}
		for name, values := range subject.Attributes {
			if !identifierPattern.MatchString(name) {
				return fmt.Errorf("principal attributes: subject %q attribute %q is not a lowercase identifier", subject.Subject, name)
			}
			if err := validateValues(values); err != nil {
				return fmt.Errorf("principal attributes: subject %q attribute %q: %w", subject.Subject, name, err)
			}
		}
		for j := range subject.Grants {
			grant := &subject.Grants[j]
			if !identifierPattern.MatchString(grant.Attribute) {
				return fmt.Errorf("principal attributes: subject %q grants[%d].attribute is not a lowercase identifier", subject.Subject, j)
			}
			if err := validateValues([]string{grant.Value}); err != nil {
				return fmt.Errorf("principal attributes: subject %q grants[%d].value: %w", subject.Subject, j, err)
			}
			from, err := time.Parse(time.RFC3339, grant.ValidFrom)
			if err != nil {
				return fmt.Errorf("principal attributes: subject %q grants[%d].valid_from must be RFC 3339", subject.Subject, j)
			}
			until, err := time.Parse(time.RFC3339, grant.ValidUntil)
			if err != nil {
				return fmt.Errorf("principal attributes: subject %q grants[%d].valid_until must be RFC 3339", subject.Subject, j)
			}
			if !until.After(from) {
				return fmt.Errorf("principal attributes: subject %q grants[%d] has an empty validity window", subject.Subject, j)
			}
			grant.validFrom, grant.validUntil = from.UTC(), until.UTC()
		}
		s.index[subject.Subject] = subject
	}
	return nil
}

func validateValues(values []string) error {
	if len(values) == 0 {
		return errors.New("at least one value is required")
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n\t") {
			return errors.New("values must be canonical non-empty tokens")
		}
		if _, duplicate := seen[value]; duplicate {
			return errors.New("values must be distinct")
		}
		seen[value] = struct{}{}
	}
	return nil
}

// digest hashes a canonical JSON rendering: subjects sorted by name, attribute
// names sorted, values sorted, grants in declared order.
func (s *Snapshot) digest() (string, error) {
	type canonicalSubject struct {
		Subject    string              `json:"subject"`
		Attributes map[string][]string `json:"attributes"`
		Grants     []Grant             `json:"grants"`
	}
	subjects := make([]canonicalSubject, 0, len(s.Subjects))
	for _, subject := range s.Subjects {
		attributes := make(map[string][]string, len(subject.Attributes))
		for name, values := range subject.Attributes {
			sorted := append([]string(nil), values...)
			sort.Strings(sorted)
			attributes[name] = sorted
		}
		grants := append([]Grant(nil), subject.Grants...)
		if grants == nil {
			grants = []Grant{}
		}
		subjects = append(subjects, canonicalSubject{Subject: subject.Subject, Attributes: attributes, Grants: grants})
	}
	sort.Slice(subjects, func(i, j int) bool { return subjects[i].Subject < subjects[j].Subject })
	encoded, err := json.Marshal(struct {
		Version  string             `json:"version"`
		Subjects []canonicalSubject `json:"subjects"`
	}{Version: s.Version, Subjects: subjects})
	if err != nil {
		return "", fmt.Errorf("encode principal attributes: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Known reports whether the subject appears in the snapshot.
func (s *Snapshot) Known(subject string) bool {
	if s == nil {
		return false
	}
	_, ok := s.index[subject]
	return ok
}

// Values returns the subject's values for an attribute at the given instant:
// standing values plus grants whose window contains the instant. The result
// is sorted and deduplicated; an unknown subject or attribute yields nil.
func (s *Snapshot) Values(subject, attribute string, now time.Time) []string {
	if s == nil {
		return nil
	}
	entry, ok := s.index[subject]
	if !ok {
		return nil
	}
	set := make(map[string]struct{})
	for _, value := range entry.Attributes[attribute] {
		set[value] = struct{}{}
	}
	instant := now.UTC()
	for _, grant := range entry.Grants {
		if grant.Attribute != attribute {
			continue
		}
		if !instant.Before(grant.validFrom) && instant.Before(grant.validUntil) {
			set[grant.Value] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}
