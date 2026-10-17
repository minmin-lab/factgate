package kbformal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// FilePrincipals renders fresh file principals for one deployment: the
// gateway's PRINCIPALS_PATH file (layout taskgate-principals-v1) for the 60
// EligibilitySubjects, and the subject -> bearer token map kb-formal-driver
// reads. Every deployment carries the same population so the profiles share
// one topology; only a profile whose attribute snapshot lists the subjects
// gives them attributes, and elsewhere they are eligible for nothing. Each
// token is 24 bytes read from random, hex encoded. The principals file holds
// only the SHA-256 of each token, so it is recordable deployment evidence; the
// token map is private to the deployment.
func FilePrincipals(random io.Reader) (principals, tokens []byte, err error) {
	type record struct {
		ID          string `json:"id"`
		Subject     string `json:"subject"`
		Role        string `json:"role"`
		TokenSHA256 string `json:"token_sha256"`
	}
	file := struct {
		Version    string   `json:"version"`
		Principals []record `json:"principals"`
	}{Version: "taskgate-principals-v1"}
	tokenMap := map[string]string{}
	for _, subject := range EligibilitySubjects() {
		raw := make([]byte, 24)
		if _, err := io.ReadFull(random, raw); err != nil {
			return nil, nil, fmt.Errorf("token for %s: %w", subject.Name, err)
		}
		token := hex.EncodeToString(raw)
		digest := sha256.Sum256([]byte(token))
		tokenMap[subject.Name] = token
		file.Principals = append(file.Principals, record{ID: "principal_" + strings.ReplaceAll(subject.Name, "-", "_"),
			Subject: subject.Name, Role: "query", TokenSHA256: hex.EncodeToString(digest[:])})
	}
	if principals, err = json.MarshalIndent(file, "", " "); err != nil {
		return nil, nil, err
	}
	if tokens, err = json.MarshalIndent(tokenMap, "", " "); err != nil {
		return nil, nil, err
	}
	return append(principals, '\n'), append(tokens, '\n'), nil
}
