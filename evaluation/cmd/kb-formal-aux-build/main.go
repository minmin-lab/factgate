// Command kb-formal-aux-build builds an auxiliary service of the
// knowledge-product formal round (today: oa-demo, the approval simulator)
// exactly the way final-v5-gateway-build builds the Gateway: from the
// tracked-file context of a clean commit equal to its origin branch, on the
// digest-pinned bases of formal-build/base-images.json, through
// Dockerfile.formal. It writes a create-exclusive manifest of the image's
// identity and a mode-0600 Compose override that pins the service to the
// immutable image ID.
//
//	kb-formal-aux-build -target oa-demo -tag T -manifest-out M -compose-override-out O
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/formalbuild"
)

const manifestVersion = "taskgate-kb-formal-aux-build-v1"

type manifest struct {
	Version              string `json:"version"`
	SubmissionCommit     string `json:"submission_commit"`
	Branch               string `json:"branch"`
	CleanTreeAtBuild     bool   `json:"clean_tree_at_build"`
	BuildContextSHA256   string `json:"build_context_sha256"`
	SourceManifestSHA256 string `json:"source_manifest_sha256"`
	BuildTarget          string `json:"build_target"`
	Service              string `json:"compose_service"`
	ImageID              string `json:"image_id"`
	Tag                  string `json:"tag"`
	Platform             string `json:"platform"`
	BuilderBaseImage     string `json:"builder_base_image"`
	RuntimeBaseImage     string `json:"runtime_base_image"`
	ModuleProxy          string `json:"module_proxy,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kb-formal-aux-build:", err)
		os.Exit(1)
	}
}

func run() error {
	root := flag.String("root", ".", "repository root")
	target := flag.String("target", "oa-demo", "auxiliary cmd/ target (formalbuild.AuxiliaryBuildTargets)")
	service := flag.String("service", "", "Compose service to pin (default: the target)")
	tag := flag.String("tag", "", "local tag for the built image")
	manifestOut := flag.String("manifest-out", "", "create the build manifest here")
	overrideOut := flag.String("compose-override-out", "", "create the Compose override here")
	flag.Parse()
	if *service == "" {
		*service = *target
	}
	if !formalbuild.AuxiliaryBuildTargets[*target] {
		return fmt.Errorf("target %q is not an admitted auxiliary target", *target)
	}
	if *tag == "" || *manifestOut == "" || *overrideOut == "" {
		return errors.New("-tag, -manifest-out and -compose-override-out are required")
	}
	for _, path := range []string{*manifestOut, *overrideOut} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("output already exists: %s", path)
		}
	}
	materialized, err := formalbuild.MaterializeHead(*root)
	if err != nil {
		return err
	}
	pins, err := formalbuild.LoadBaseImagePins(filepath.Join(*root, formalbuild.BaseImagePinPath))
	if err != nil {
		return err
	}
	if err := pins.Pinned(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	socket, err := formalbuild.DockerSocket(os.Getenv)
	if err != nil {
		return err
	}
	engine, err := formalbuild.NewHTTPEngine(ctx, socket)
	if err != nil {
		return err
	}
	for _, pin := range pins.Images {
		reference, err := pin.Reference()
		if err != nil {
			return err
		}
		if _, err := engine.ImageInspect(ctx, reference); err != nil {
			return fmt.Errorf("the pinned %s base image %s is not present locally: %w", pin.Role, reference, err)
		}
	}
	moduleProxy := strings.TrimSpace(os.Getenv("TASKGATE_FORMAL_BUILD_GOPROXY"))
	fmt.Printf("formal %s build\n  commit   %s (clean, equals origin/%s)\n  context  %s over %d tracked files\n",
		*target, materialized.Source.Commit[:12], materialized.Source.Branch, materialized.ContextSHA256[:12], len(materialized.Entries))
	image, err := formalbuild.Build(ctx, engine, formalbuild.ExecBuilder, formalbuild.BuildRequest{
		Context: materialized, Pins: pins, Tag: *tag, ModuleProxy: moduleProxy, Target: *target,
	})
	if err != nil {
		return err
	}
	builder, _ := image.Label(formalbuild.LabelBuilderBaseImage)
	runtime, _ := image.Label(formalbuild.LabelRuntimeBaseImage)
	record := manifest{Version: manifestVersion, SubmissionCommit: materialized.Source.Commit, Branch: materialized.Source.Branch,
		CleanTreeAtBuild: true, BuildContextSHA256: materialized.ContextSHA256, SourceManifestSHA256: materialized.SourceManifestSHA256,
		BuildTarget: *target, Service: *service, ImageID: image.ID, Tag: *tag, Platform: image.Platform(),
		BuilderBaseImage: builder, RuntimeBaseImage: runtime, ModuleProxy: moduleProxy}
	encoded, err := json.MarshalIndent(record, "", " ")
	if err != nil {
		return err
	}
	if err := writeExclusive(*manifestOut, append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	override := fmt.Sprintf("services:\n  %s:\n    image: %s\n    pull_policy: never\n", *service, image.ID)
	if err := writeExclusive(*overrideOut, []byte(override), 0o600); err != nil {
		return err
	}
	fmt.Printf("  image    %s (%s)\n", image.ID, image.Platform())
	return nil
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
