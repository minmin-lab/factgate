package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"taskbound.local/agent-data-gateway/internal/control"
)

// principalsFileVersion names the only accepted layout of PRINCIPALS_PATH.
const principalsFileVersion = "taskgate-principals-v1"

// maxFilePrincipals bounds the file so a malformed deployment cannot turn
// startup into an unbounded write loop against the Control store.
const maxFilePrincipals = 10000

type principalsFile struct {
	Version    string            `json:"version"`
	Principals []principalRecord `json:"principals"`
}

type principalRecord struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Role        string `json:"role"`
	TokenSHA256 string `json:"token_sha256"`
}

var (
	filePrincipalID      = regexp.MustCompile(`^principal_[a-z0-9_]{1,60}$`)
	filePrincipalSubject = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
	lowerSHA256Hex       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// loadPrincipalsFile reads the optional PRINCIPALS_PATH file: additional query
// principals that the gateway bootstraps at startup exactly as it bootstraps
// Alice and Carol from their token variables. The file carries only the
// SHA-256 of each bearer token, never a token, so it can be recorded as
// deployment evidence. It is refused as a whole when it has unknown keys or
// trailing data, another version, no entries, more than maxFilePrincipals
// entries, a role other than query, a malformed identifier or digest, or a
// duplicate or reserved ID, subject or token digest.
func loadPrincipalsFile(path string, reserved []control.Principal) ([]control.Principal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read principals file: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file principalsFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode principals file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("principals file has trailing data after its JSON object")
	}
	if file.Version != principalsFileVersion {
		return nil, fmt.Errorf("principals file version %q is not %s", file.Version, principalsFileVersion)
	}
	if len(file.Principals) == 0 {
		return nil, errors.New("principals file lists no principals")
	}
	if len(file.Principals) > maxFilePrincipals {
		return nil, fmt.Errorf("principals file lists %d principals, more than %d", len(file.Principals), maxFilePrincipals)
	}
	ids := make(map[string]struct{}, len(file.Principals)+len(reserved))
	subjects := make(map[string]struct{}, len(file.Principals)+len(reserved))
	tokens := make(map[string]struct{}, len(file.Principals)+len(reserved))
	for _, principal := range reserved {
		ids[principal.ID] = struct{}{}
		subjects[principal.Subject] = struct{}{}
		tokens[principal.TokenHash] = struct{}{}
	}
	principals := make([]control.Principal, 0, len(file.Principals))
	for index, record := range file.Principals {
		switch {
		case !filePrincipalID.MatchString(record.ID):
			return nil, fmt.Errorf("principals[%d]: id %q is not principal_<lowercase>", index, record.ID)
		case !filePrincipalSubject.MatchString(record.Subject):
			return nil, fmt.Errorf("principals[%d]: subject %q is not a lowercase identifier", index, record.Subject)
		case record.Role != "query":
			return nil, fmt.Errorf("principals[%d]: role %q is not query", index, record.Role)
		case !lowerSHA256Hex.MatchString(record.TokenSHA256):
			return nil, fmt.Errorf("principals[%d]: token_sha256 is not a lowercase SHA-256", index)
		}
		if _, taken := ids[record.ID]; taken {
			return nil, fmt.Errorf("principals[%d]: id %q is duplicate or reserved", index, record.ID)
		}
		if _, taken := subjects[record.Subject]; taken {
			return nil, fmt.Errorf("principals[%d]: subject %q is duplicate or reserved", index, record.Subject)
		}
		if _, taken := tokens[record.TokenSHA256]; taken {
			return nil, fmt.Errorf("principals[%d]: token digest is duplicate or reserved", index)
		}
		ids[record.ID] = struct{}{}
		subjects[record.Subject] = struct{}{}
		tokens[record.TokenSHA256] = struct{}{}
		principals = append(principals, control.Principal{ID: record.ID, Subject: record.Subject, Role: record.Role, TokenHash: record.TokenSHA256})
	}
	return principals, nil
}
