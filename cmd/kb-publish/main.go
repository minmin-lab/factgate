// Command kb-publish freezes one document collection into one knowledge-product
// index publication epoch (docs/kb-design.md): it loads kb.* rows, creates the
// frozen reporting views, compiles the three V4 publication bundles with the
// snapshot compiler, and writes the digest-bound manifest, the compiler inputs
// (for snapshot-index / snapshot-sidecar-install) and a Catalog fragment.
//
//	kb-publish -spec spec.json -out ./out            # publish (KB_ADMIN_POSTGRES_DSN, SNAPSHOT_POSTGRES_DSN)
//	kb-publish -verify -out ./out                    # rescan + recompile + compare (SNAPSHOT_POSTGRES_DSN)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"taskbound.local/agent-data-gateway/internal/kbpublish"
)

func main() {
	specPath := flag.String("spec", "", "path to a taskgate-kb-publish-spec-v1 JSON file")
	out := flag.String("out", "", "output directory (bundles, inputs, kb-publication.json, catalog-fragment.yaml)")
	verify := flag.Bool("verify", false, "verify an existing output directory against the database instead of publishing")
	adminEnv := flag.String("admin-dsn-env", "KB_ADMIN_POSTGRES_DSN", "environment variable holding the administrator DSN (publish only)")
	readerEnv := flag.String("reader-dsn-env", "SNAPSHOT_POSTGRES_DSN", "environment variable holding the read-only scanner DSN")
	flag.Parse()
	if *out == "" || flag.NArg() != 0 || (!*verify && *specPath == "") {
		flag.Usage()
		os.Exit(2)
	}
	readerDSN := strings.TrimSpace(os.Getenv(*readerEnv))
	if readerDSN == "" {
		fatal(fmt.Errorf("%s is required", *readerEnv))
	}
	ctx := context.Background()
	if *verify {
		manifest, err := kbpublish.Verify(ctx, *out, readerDSN)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("verified knowledge_product=%s epoch=%s manifest_digest=%s\n", manifest.KnowledgeProduct, manifest.Epoch, manifest.ManifestDigest)
		return
	}
	spec, err := kbpublish.LoadSpec(*specPath)
	if err != nil {
		fatal(err)
	}
	adminDSN := strings.TrimSpace(os.Getenv(*adminEnv))
	if adminDSN == "" {
		fatal(fmt.Errorf("%s is required", *adminEnv))
	}
	result, err := kbpublish.Publish(ctx, spec, kbpublish.Options{AdminDSN: adminDSN, ReaderDSN: readerDSN, OutputDir: *out})
	if err != nil {
		fatal(err)
	}
	counts := result.Manifest.Counts
	fmt.Printf("published knowledge_product=%s epoch=%s documents=%d versions=%d chunks=%d leadership_chunks=%d values=%d manifest_digest=%s\n",
		result.Manifest.KnowledgeProduct, result.Manifest.Epoch, counts.Documents, counts.DocumentVersions, counts.Chunks,
		counts.LeadershipChunks, counts.Values, result.Manifest.ManifestDigest)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "kb-publish:", err)
	os.Exit(1)
}
