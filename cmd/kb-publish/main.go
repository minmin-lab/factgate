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
	"path/filepath"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
)

func main() {
	specPath := flag.String("spec", "", "path to a taskgate-kb-publish-spec-v1 JSON file")
	artifactDir := flag.String("artifact-dir", "", "snapshot artifact root: bundles land in <dir>/<publication>/, the manifest in <dir>/kb/<epoch>/")
	out := flag.String("out", "", "explicit output directory (overrides -artifact-dir; bundles in <out>/bundles)")
	verify := flag.Bool("verify", false, "verify an existing output directory against the database instead of publishing")
	merge := flag.Bool("merge", false, "merge -fragment into -base and write the validated candidate Catalog to -out")
	activate := flag.Bool("activate", false, "run the gateway's knowledge-product activation check for -catalog against -artifact-dir")
	catalogPath := flag.String("catalog", "", "activate: the candidate Catalog to check")
	basePath := flag.String("base", "config/catalog.yaml", "merge: the Catalog to extend")
	fragmentPath := flag.String("fragment", "", "merge: the catalog-fragment.yaml a publish produced")
	adminEnv := flag.String("admin-dsn-env", "KB_ADMIN_POSTGRES_DSN", "environment variable holding the administrator DSN (publish only)")
	readerEnv := flag.String("reader-dsn-env", "SNAPSHOT_POSTGRES_DSN", "environment variable holding the read-only scanner DSN")
	flag.Parse()
	if *activate {
		if *catalogPath == "" || *artifactDir == "" || flag.NArg() != 0 {
			flag.Usage()
			os.Exit(2)
		}
		loaded, err := catalog.Load(*catalogPath)
		if err != nil {
			fatal(err)
		}
		manifests, err := kbpublish.ActivateFromDirectory(loaded, *artifactDir)
		if err != nil {
			fatal(err)
		}
		for _, manifest := range manifests {
			fmt.Printf("activated knowledge_product=%s epoch=%s manifest_digest=%s chunks=%d\n", manifest.KnowledgeProduct, manifest.Epoch, manifest.ManifestDigest, manifest.Counts.Chunks)
		}
		return
	}
	if *merge {
		if *out == "" || *fragmentPath == "" || flag.NArg() != 0 {
			flag.Usage()
			os.Exit(2)
		}
		mergeCatalog(*basePath, *fragmentPath, *out)
		return
	}
	if (*out == "" && *artifactDir == "") || flag.NArg() != 0 || (!*verify && *specPath == "") {
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
	options := kbpublish.Options{AdminDSN: adminDSN, ReaderDSN: readerDSN, OutputDir: *out}
	if *out == "" {
		options.OutputDir = filepath.Join(*artifactDir, "kb", spec.Epoch)
		options.BundleDir = *artifactDir
	}
	result, err := kbpublish.Publish(ctx, spec, options)
	if err != nil {
		fatal(err)
	}
	counts := result.Manifest.Counts
	fmt.Printf("published knowledge_product=%s epoch=%s documents=%d versions=%d chunks=%d leadership_chunks=%d values=%d manifest_digest=%s\n",
		result.Manifest.KnowledgeProduct, result.Manifest.Epoch, counts.Documents, counts.DocumentVersions, counts.Chunks,
		counts.LeadershipChunks, counts.Values, result.Manifest.ManifestDigest)
}

// mergeCatalog appends the fragment's top-level sequences to the base Catalog
// and refuses to write a candidate that does not validate.
func mergeCatalog(basePath, fragmentPath, outPath string) {
	base, err := os.ReadFile(basePath)
	if err != nil {
		fatal(err)
	}
	fragment, err := os.ReadFile(fragmentPath)
	if err != nil {
		fatal(err)
	}
	merged, err := catalog.MergeYAMLFragment(base, fragment)
	if err != nil {
		fatal(err)
	}
	parsed, err := catalog.Parse(merged)
	if err != nil {
		fatal(fmt.Errorf("merged candidate Catalog does not validate: %w", err))
	}
	if err := os.WriteFile(outPath, merged, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("merged catalog_version=%s knowledge_products=%d sha256=%s -> %s\n", parsed.CatalogVersion, len(parsed.KnowledgeProducts), parsed.SHA256, outPath)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "kb-publish:", err)
	os.Exit(1)
}
