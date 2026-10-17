package kbformal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCacheContentDigestMatchesTheSha256sumListing(t *testing.T) {
	dir := t.TempDir()
	write := func(relative, content string) {
		path := filepath.Join(dir, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("b/2.f32le", "two")
	write("a/1.f32le", "one")
	write("a/notes.txt", "ignored")
	if err := os.Symlink(filepath.Join(dir, "a/1.f32le"), filepath.Join(dir, "a/link.f32le")); err != nil {
		t.Fatal(err)
	}
	hexOf := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		return hex.EncodeToString(sum[:])
	}
	listing := fmt.Sprintf("%s  a/1.f32le\n%s  b/2.f32le\n", hexOf("one"), hexOf("two"))
	digest, files, err := CacheContentDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 || digest != hexOf(listing) {
		t.Fatalf("digest %s over %d files, want %s over 2", digest, files, hexOf(listing))
	}
	write("b/2.f32le", "changed")
	if changed, _, _ := CacheContentDigest(dir); changed == digest {
		t.Fatal("a changed vector file kept the digest")
	}
}

func TestComposeImagesReadsTheKBOverlayPins(t *testing.T) {
	images, err := composeImages(filepath.Join("..", "kb-formal", "compose.kb.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"control-postgres", "business-postgres", "result-object-store", "result-object-store-init", "kb-embed"} {
		if len(images[service]) < len("@sha256:")+64 {
			t.Fatalf("service %s: pinned image %q", service, images[service])
		}
	}
	if _, ok := images["gateway"]; ok {
		t.Fatal("the gateway image comes from the formal build override, not the overlay")
	}
}
