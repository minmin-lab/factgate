// Command kb-formal-profile generates (or verifies) the profile Catalogs,
// principal attribute snapshots and registry of the knowledge-product formal
// round from the frozen publications under evaluation/kb-formal/publications.
//
//	kb-formal-profile -write    regenerate evaluation/kb-formal/profiles
//	kb-formal-profile -verify   fail unless the committed files regenerate byte for byte
//	kb-formal-profile -attest -profile NAME [-record]
//	                            attest the live reporting surface of a knowledge-product
//	                            business database (reader DSN in $KB_FORMAL_READER_DSN)
//	                            against the profile Catalog, and optionally record the
//	                            digest in profiles/schema-attestations.json
//	kb-formal-profile -principals DIR
//	                            write fresh file principals for one deployment:
//	                            DIR/principals.json (mode 0644, token digests only) and
//	                            DIR/principal-tokens.json (mode 0600, the bearer tokens)
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/kbformal"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

func main() {
	write := flag.Bool("write", false, "write the generated files")
	verify := flag.Bool("verify", false, "verify the committed files regenerate byte for byte")
	root := flag.String("root", ".", "repository root")
	attest := flag.Bool("attest", false, "attest a live business database against a profile Catalog")
	profileName := flag.String("profile", "", "attest: the profile")
	dsnEnv := flag.String("dsn-env", "KB_FORMAL_READER_DSN", "attest: environment variable holding the read-only business DSN")
	record := flag.Bool("record", false, "attest: record the digest in profiles/schema-attestations.json")
	principalsDir := flag.String("principals", "", "write fresh file principals and their tokens into this directory")
	flag.Parse()
	if *principalsDir != "" {
		if err := writePrincipals(*principalsDir); err != nil {
			fmt.Fprintln(os.Stderr, "kb-formal-profile:", err)
			os.Exit(1)
		}
		return
	}
	if *attest {
		if err := attestProfile(*root, *profileName, *dsnEnv, *record); err != nil {
			fmt.Fprintln(os.Stderr, "kb-formal-profile:", err)
			os.Exit(1)
		}
		return
	}
	if *write == *verify {
		fmt.Fprintln(os.Stderr, "kb-formal-profile: exactly one of -write or -verify is required")
		os.Exit(2)
	}
	files, err := kbformal.Generate(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kb-formal-profile:", err)
		os.Exit(1)
	}
	failed := false
	for _, file := range files {
		if *write {
			if err := os.MkdirAll(filepath.Dir(file.Path), 0o755); err != nil {
				fmt.Fprintln(os.Stderr, "kb-formal-profile:", err)
				os.Exit(1)
			}
			if err := os.WriteFile(file.Path, file.Data, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, "kb-formal-profile:", err)
				os.Exit(1)
			}
			fmt.Printf("wrote %s\n", file.Path)
			continue
		}
		committed, err := os.ReadFile(file.Path)
		if err != nil || !bytes.Equal(committed, file.Data) {
			fmt.Fprintf(os.Stderr, "kb-formal-profile: %s does not regenerate byte for byte\n", file.Path)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
	if *verify {
		fmt.Printf("kb-formal-profile: %d files regenerate byte for byte\n", len(files))
	}
}

// attestProfile computes the live reporting-surface digest the gateway's data
// connector would compute for the profile's products (phase 1 of the pilot
// runners' two-phase attestation): the expected schema comes from the profile
// Catalog without an attested digest, and the connector reads only catalog
// metadata, never rows.
func attestProfile(root, name, dsnEnv string, record bool) error {
	profile, ok := kbformal.LookupProfile(name)
	if !ok {
		return fmt.Errorf("unknown profile %q", name)
	}
	dsn := strings.TrimSpace(os.Getenv(dsnEnv))
	if dsn == "" {
		return fmt.Errorf("%s is required", dsnEnv)
	}
	text, err := kbformal.ProfileCatalog(root, profile, "")
	if err != nil {
		return err
	}
	parsed, err := catalog.Parse(text)
	if err != nil {
		return err
	}
	built, err := catalogschema.Build(parsed)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	connector, err := dataconnector.New(ctx, dataconnector.Config{DSN: dsn, StatementTimeout: 10 * time.Second, ConnectTimeout: 5 * time.Second,
		MaxRows: 500, MaxConnections: 2, ExpectedSchema: built.Entries})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer connector.Close()
	attestation, err := connector.Attestation(ctx)
	if err != nil {
		return fmt.Errorf("attest: %w", err)
	}
	fmt.Printf("profile=%s epoch=%s schema_digest=%s entries=%d\n", profile.Name, profile.Epoch, attestation.SchemaDigest, len(built.Entries))
	if !record {
		return nil
	}
	attestations, err := kbformal.LoadAttestations(root)
	if err != nil {
		return err
	}
	attestations.Profiles[profile.Name] = attestation.SchemaDigest
	attestations.Note = "Reporting-surface digests of the knowledge-product business database, from kb-formal-profile -attest against a fresh formal deployment; every formal deployment's live attestation must equal them."
	encoded, err := json.MarshalIndent(attestations, "", " ")
	if err != nil {
		return err
	}
	path := filepath.Join(kbformal.ProfilesDir(root), "schema-attestations.json")
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

// writePrincipals creates the two files exclusively, so a deployment's
// principals are never overwritten, and sets each mode explicitly so the umask
// cannot make the principals file unreadable to the gateway's user.
func writePrincipals(dir string) error {
	principals, tokens, err := kbformal.FilePrincipals(rand.Reader)
	if err != nil {
		return err
	}
	for _, file := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{{"principal-tokens.json", tokens, 0o600}, {"principals.json", principals, 0o644}} {
		handle, err := os.OpenFile(filepath.Join(dir, file.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := handle.Write(file.data); err != nil {
			handle.Close()
			return err
		}
		if err := handle.Chmod(file.mode); err != nil {
			handle.Close()
			return err
		}
		if err := handle.Close(); err != nil {
			return err
		}
	}
	return nil
}
