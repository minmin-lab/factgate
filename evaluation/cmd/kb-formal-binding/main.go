// Command kb-formal-binding writes or verifies the KB dataset binding of a
// formal round (docs/kb_experiment_design.md, 正式轮规程 §2): the digests of
// every frozen input the round consumes, in the round's binding file
// (kbformal.Rounds: kb -> kb-dataset-binding-v1.json, keyword ->
// kb-dataset-binding-v2.json). The author approves the SHA-256 of that file
// before the launch; the formal gateway build seals it as -dataset-binding,
// and the launcher runs -verify on the execution host.
//
//	kb-formal-binding -round R -write      regenerate the binding from the current inputs
//	kb-formal-binding -round R -verify     fail unless the current inputs give the committed bytes
//	kb-formal-binding -round R -path       print the round's binding path
//	kb-formal-binding -round R -profiles   print the round's profiles, space-separated
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/kbformal"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kb-formal-binding:", err)
		os.Exit(1)
	}
}

func run() error {
	write := flag.Bool("write", false, "write the binding")
	verify := flag.Bool("verify", false, "verify the committed binding")
	roundName := flag.String("round", "kb", "round: kb | keyword")
	printPath := flag.Bool("path", false, "print the round's binding path and exit")
	printProfiles := flag.Bool("profiles", false, "print the round's profiles and exit")
	root := flag.String("root", ".", "repository root")
	modelDir := flag.String("model-dir", "/home/wmm/kb-embed/models/bge-m3", "embedding model directory (holds pytorch_model.bin)")
	cacheDir := flag.String("cache-dir", "/home/wmm/kb-embed/cache", "embedding cache directory")
	venvPython := flag.String("venv-python", "/home/wmm/kb-embed/venv/bin/python", "the frozen embedding venv's interpreter")
	flag.Parse()
	round, err := kbformal.LookupRound(*roundName)
	if err != nil {
		return err
	}
	if *printPath {
		fmt.Println(round.BindingPath)
		return nil
	}
	if *printProfiles {
		fmt.Println(strings.Join(round.Profiles, " "))
		return nil
	}
	if *write == *verify {
		return fmt.Errorf("exactly one of -write or -verify is required")
	}
	binding, err := kbformal.BuildRoundBinding(*root, kbformal.BindingHost{ModelDir: *modelDir, CacheDir: *cacheDir, VenvPython: *venvPython}, round)
	if err != nil {
		return err
	}
	path := filepath.Join(*root, round.BindingPath)
	if *verify {
		committed, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(committed, binding) {
			return fmt.Errorf("%s does not match the current inputs", round.BindingPath)
		}
	} else if err := os.WriteFile(path, binding, 0o644); err != nil {
		return err
	}
	sum := sha256.Sum256(binding)
	fmt.Printf("%s sha256=%s bytes=%d\n", round.BindingPath, hex.EncodeToString(sum[:]), len(binding))
	return nil
}
