package formalbuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// FormalDockerfile is the tracked path of the formal build recipe, relative to
// the context root. It must be tracked: the context is a git archive, so an
// untracked Dockerfile could not be found inside it.
const FormalDockerfile = "Dockerfile.formal"

// BaseImagePinPath is the tracked path of the digest pin document.
const BaseImagePinPath = "formal-build/base-images.json"

// Builder runs one external command with an optional stdin body.
//
// Injected rather than called directly so the argument construction — which is
// where a missing --build-arg would silently produce an unlabelled image — can
// be tested without a Docker daemon.
type Builder func(ctx context.Context, stdin io.Reader, name string, arguments ...string) ([]byte, error)

// ExecBuilder runs commands with os/exec.
func ExecBuilder(ctx context.Context, stdin io.Reader, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Stdin = stdin
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(arguments, " "), err,
			strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

// BuildRequest is one formal Gateway build.
type BuildRequest struct {
	// Context is the materialized tracked-file-only context.
	Context Context
	// Pins are the digest-pinned base images.
	Pins BaseImagePins
	// Tag is the local name to give the built image. It is a convenience for the
	// operator; nothing downstream trusts it, because a tag can be moved.
	Tag string
	// ModuleProxy, when set, is passed to the builder as GOPROXY. It changes
	// only where modules are fetched from: go.sum still decides which bytes are
	// accepted. Empty leaves the Dockerfile's default in force.
	ModuleProxy string
	// Target is the cmd/ binary to build. Empty means the Gateway, which is the
	// only target the Gateway manifest, runtime identity and V5 campaign accept;
	// AuxiliaryBuildTargets lists the others a deployment may build the same way.
	Target string
}

// AuxiliaryBuildTargets are the non-Gateway services a formal deployment may
// build from the same tracked-file context and pinned bases (the knowledge-
// product formal round builds its approval simulator this way).
var AuxiliaryBuildTargets = map[string]bool{"oa-demo": true}

func (request BuildRequest) target() (string, error) {
	if request.Target == "" || request.Target == GatewayBuildTarget {
		return GatewayBuildTarget, nil
	}
	if !AuxiliaryBuildTargets[request.Target] {
		return "", fmt.Errorf("formal build target %q is neither the Gateway nor an admitted auxiliary target", request.Target)
	}
	return request.Target, nil
}

// BuildArguments renders the exact docker build argv.
//
// The context is streamed on stdin as a tar, which is what makes the build
// tracked-file-only: there is no host directory for the builder to read, so an
// untracked or ignored file has no path into the image.
func (request BuildRequest) BuildArguments() ([]string, error) {
	if err := request.Pins.Pinned(); err != nil {
		return nil, err
	}
	if !validCommitSHA1(request.Context.Source.Commit) {
		return nil, errors.New("the formal build has no materialized commit")
	}
	if err := request.Context.Source.RequireBuildable(); err != nil {
		return nil, err
	}
	if !validSHA256(request.Context.ContextSHA256) || !validSHA256(request.Context.SourceManifestSHA256) {
		return nil, errors.New("the formal build context has not been digested")
	}
	if strings.TrimSpace(request.Tag) == "" {
		return nil, errors.New("the formal build needs a local tag to refer to the result")
	}
	if strings.ContainsAny(request.ModuleProxy, " \t\r\n") {
		return nil, errors.New("the formal build module proxy must be a single GOPROXY value without whitespace")
	}
	target, err := request.target()
	if err != nil {
		return nil, err
	}
	builder, err := request.Pins.Pin(BuilderRole)
	if err != nil {
		return nil, err
	}
	runtime, err := request.Pins.Pin(RuntimeRole)
	if err != nil {
		return nil, err
	}
	builderReference, err := builder.Reference()
	if err != nil {
		return nil, err
	}
	runtimeReference, err := runtime.Reference()
	if err != nil {
		return nil, err
	}
	arguments := []string{
		"build",
		"--file", FormalDockerfile,
		"--target", "runtime",
		"--build-arg", "TARGET=" + target,
		"--build-arg", "SOURCE_COMMIT=" + request.Context.Source.Commit,
		"--build-arg", "BUILD_CONTEXT_SHA256=" + request.Context.ContextSHA256,
		"--build-arg", "SOURCE_MANIFEST_SHA256=" + request.Context.SourceManifestSHA256,
		"--build-arg", "BUILDER_BASE_IMAGE=" + builderReference,
		"--build-arg", "RUNTIME_BASE_IMAGE=" + runtimeReference,
	}
	if request.ModuleProxy != "" {
		arguments = append(arguments, "--build-arg", "GOPROXY="+request.ModuleProxy)
	}
	return append(arguments,
		"--tag", request.Tag,
		// A build that reused a layer cached from a different context would defeat
		// the point of digesting the context at all.
		"--no-cache",
		"--pull=false",
		"-",
	), nil
}

// Build streams the context to the builder and returns the built image ID.
//
// The ID is read back through Docker Engine rather than parsed out of the
// builder's log, because the log is a human-readable stream whose format is not
// part of any contract.
func Build(ctx context.Context, engine Engine, builder Builder, request BuildRequest) (ImageInspect, error) {
	target, err := request.target()
	if err != nil {
		return ImageInspect{}, err
	}
	arguments, err := request.BuildArguments()
	if err != nil {
		return ImageInspect{}, err
	}
	if _, err := builder(ctx, bytes.NewReader(request.Context.Archive), "docker", arguments...); err != nil {
		return ImageInspect{}, fmt.Errorf("formal Gateway build: %w", err)
	}
	image, err := engine.ImageInspect(ctx, request.Tag)
	if err != nil {
		return ImageInspect{}, err
	}
	if err := RequireFormalImageForTarget(image, FromContext(request.Context), target); err != nil {
		return ImageInspect{}, err
	}
	return image, nil
}

// RequireFormalImage rejects an image whose labels do not describe the source it
// was supposed to be built from.
//
// This runs on the freshly built image, before anything is deployed from it, so
// a build invoked without one of its build arguments fails at the build rather
// than at the observer snapshot hours later.
func RequireFormalImage(image ImageInspect, expected ExpectedSource) error {
	return RequireFormalImageForTarget(image, expected, GatewayBuildTarget)
}

// RequireFormalImageForTarget is RequireFormalImage for an explicit build
// target (the Gateway or an admitted auxiliary target).
func RequireFormalImageForTarget(image ImageInspect, expected ExpectedSource, target string) error {
	if _, present := image.Label(LabelFormalBuild); !present {
		return fmt.Errorf("image %s carries no %s label", shortImageID(image.ID), LabelFormalBuild)
	}
	for _, check := range []struct{ name, want string }{
		{LabelRevision, expected.Commit},
		{LabelBuildContext, expected.ContextSHA256},
		{LabelSourceManifest, expected.SourceManifestSHA256},
		{LabelBuildTarget, target},
	} {
		got, present := image.Label(check.name)
		if !present {
			return fmt.Errorf("image %s omits the %s label", shortImageID(image.ID), check.name)
		}
		if got != check.want {
			return fmt.Errorf("image %s labels %s as %q, the formal build names %q",
				shortImageID(image.ID), check.name, got, check.want)
		}
	}
	if image.Platform() == "/" || strings.TrimSpace(image.Platform()) == "" {
		return fmt.Errorf("image %s reports no platform", shortImageID(image.ID))
	}
	return nil
}

// ResolveBaseImageDigest resolves one tag to its registry content digest.
//
// Used only by the record path. The build path never calls it: resolving a tag
// at build time is exactly the mutable-pointer problem the pins exist to remove.
func ResolveBaseImageDigest(ctx context.Context, engine Engine, builder Builder, tag string) (string, error) {
	if _, err := builder(ctx, nil, "docker", "pull", "--quiet", tag); err != nil {
		return "", fmt.Errorf("pull %s: %w", tag, err)
	}
	image, err := engine.ImageInspect(ctx, tag)
	if err != nil {
		return "", err
	}
	repository, _, found := strings.Cut(tag, ":")
	if !found || repository == "" {
		return "", fmt.Errorf("base image tag %q has no repository", tag)
	}
	var digests []string
	for _, repoDigest := range image.RepoDigests {
		name, digest, split := strings.Cut(repoDigest, "@")
		if !split || name != repository {
			continue
		}
		if !validDigestReference(digest) {
			return "", fmt.Errorf("%s reports a malformed repo digest %q", tag, repoDigest)
		}
		digests = append(digests, digest)
	}
	switch len(digests) {
	case 1:
		return digests[0], nil
	case 0:
		return "", fmt.Errorf("%s has no registry digest locally; it was built or loaded rather than pulled, "+
			"so there is nothing immutable to pin", tag)
	default:
		// More than one digest for one repository means the local store cannot
		// say which bytes the tag names.
		return "", fmt.Errorf("%s resolves to %d distinct registry digests locally; the pin would be ambiguous",
			tag, len(digests))
	}
}
