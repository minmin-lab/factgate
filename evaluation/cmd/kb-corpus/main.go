// Command kb-corpus generates the frozen synthetic knowledge-product corpus
// (evaluation/kbcorpus) into a raw directory and writes its digest manifest.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
)

func main() {
	seed := flag.Uint64("seed", 20261015, "generator seed (part of the frozen manifest)")
	documents := flag.Int("documents", 10000, "number of documents (versions are added by the generator)")
	out := flag.String("out", "evaluation/kbcorpus/raw/v1", "output directory for documents.jsonl and manifest.json")
	freeze := flag.String("freeze", "", "also write the manifest to this path (the committed digest binding)")
	flag.Parse()
	spec := kbcorpus.Spec{Seed: *seed, Documents: *documents}
	_, jsonl, manifest, err := kbcorpus.Build(spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kb-corpus:", err)
		os.Exit(1)
	}
	encodedManifest, err := kbcorpus.EncodeManifest(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kb-corpus:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "kb-corpus:", err)
		os.Exit(1)
	}
	for name, data := range map[string][]byte{"documents.jsonl": jsonl, "manifest.json": encodedManifest} {
		if err := os.WriteFile(filepath.Join(*out, name), data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "kb-corpus:", err)
			os.Exit(1)
		}
	}
	if *freeze != "" {
		if err := os.WriteFile(*freeze, encodedManifest, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "kb-corpus:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("documents=%d versions=%d sections=%d leadership_sections=%d bytes=%d sha256=%s\n",
		manifest.Counts.Documents, manifest.Counts.Versions, manifest.Counts.Sections,
		manifest.Counts.LeadershipSections, manifest.DocumentsBytes, manifest.DocumentsSHA256)
}
