package kbformal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

// The KB dataset binding (docs/kb_experiment_design.md, 正式轮规程 §2) names,
// by digest, every frozen input the formal round consumes: the corpora and
// their manifests, the frozen publications with their publish specs, the
// embedding model weights, the embedding cache content, the embedding
// service's Python environment, the pinned images, the query set, the four
// profiles, and the source files that build the knowledge-product topology.
// The author approves the SHA-256 of the written file; the formal gateway
// build seals it as -dataset-binding. It carries no timestamp, so the same
// inputs always give the same bytes.

const (
	BindingVersion = "taskgate-kb-dataset-binding-v1"
	BindingPath    = "evaluation/kb-formal/kb-dataset-binding-v1.json"
)

// BindingFile is one file by repository-relative path, digest and size.
type BindingFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// BindingHost locates the execution-host inputs that are not in the
// repository: the model directory, the embedding cache and the frozen venv.
type BindingHost struct {
	ModelDir   string
	CacheDir   string
	VenvPython string
}

// Binding is the document.
type Binding struct {
	Version      string               `json:"version"`
	Note         string               `json:"note"`
	Corpora      []BindingCorpus      `json:"corpora"`
	Publications []BindingPublication `json:"publications"`
	Embedding    BindingEmbedding     `json:"embedding"`
	Images       map[string]string    `json:"images"`
	QuerySet     []BindingFile        `json:"query_set"`
	KeywordSet   []BindingFile        `json:"keyword_set,omitempty"`
	Profiles     []BindingProfile     `json:"profiles"`
	ProfileFiles []BindingFile        `json:"profile_files"`
	SourceFiles  []BindingFile        `json:"source_files"`
}

// BindingCorpus is one corpus file and the manifest beside it.
type BindingCorpus struct {
	Documents BindingFile `json:"documents"`
	Manifest  BindingFile `json:"manifest"`
	Epochs    []string    `json:"epochs"`
}

// BindingPublication is one frozen publication directory.
type BindingPublication struct {
	Epoch string        `json:"epoch"`
	Files []BindingFile `json:"files"`
}

// BindingEmbedding identifies the embedding model and everything derived
// from it outside the repository.
type BindingEmbedding struct {
	Model              string         `json:"model"`
	Dimension          int            `json:"dimension"`
	WeightsFile        string         `json:"weights_file"`
	WeightsSHA256      string         `json:"weights_sha256"`
	CacheFiles         int            `json:"cache_files"`
	CacheContentSHA256 string         `json:"cache_content_sha256"`
	Environment        map[string]any `json:"python_environment"`
}

// BindingProfile is one profile's attested facts.
type BindingProfile struct {
	Name          string `json:"name"`
	Epoch         string `json:"epoch"`
	CatalogSHA256 string `json:"catalog_sha256"`
	SchemaDigest  string `json:"schema_digest"`
}

func bindingFile(root, relative string) (BindingFile, error) {
	data, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return BindingFile{}, err
	}
	sum := sha256.Sum256(data)
	return BindingFile{Path: relative, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data))}, nil
}

func sha256OfFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// CacheContentDigest is the content digest of an embedding cache: every
// regular *.f32le file, by path relative to dir in byte order, hashed as the
// lines `sha256sum` prints ("<hex>  <path>\n"), and that text hashed again.
// It equals the digest run-deployment.sh takes before and after each KB load.
func CacheContentDigest(dir string) (string, int, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".f32le") {
			relative, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			paths = append(paths, relative)
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	sort.Strings(paths)
	listing := sha256.New()
	for _, relative := range paths {
		digest, err := sha256OfFile(filepath.Join(dir, relative))
		if err != nil {
			return "", 0, err
		}
		fmt.Fprintf(listing, "%s  %s\n", digest, relative)
	}
	return hex.EncodeToString(listing.Sum(nil)), len(paths), nil
}

var composeImage = regexp.MustCompile(`^\s+image:\s*(\S+@sha256:[0-9a-f]{64})\s*$`)

// composeImages returns the digest-pinned image of each service in the KB
// overlay, keyed by service name.
func composeImages(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	images := map[string]string{}
	service := ""
	inServices := false
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case line == "services:":
			inServices = true
		case len(line) > 0 && line[0] != ' ' && line[0] != '#':
			inServices = false
		case inServices && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(strings.TrimSpace(line), ":"):
			service = strings.TrimSuffix(strings.TrimSpace(line), ":")
		case inServices && service != "":
			if match := composeImage.FindStringSubmatch(line); match != nil {
				images[service] = match[1]
			}
		}
	}
	return images, nil
}

// BuildBinding is BuildRoundBinding for the first round (kb).
func BuildBinding(root string, host BindingHost) ([]byte, error) {
	return BuildRoundBinding(root, host, Rounds["kb"])
}

// BuildRoundBinding reads every input of a round and returns the binding's
// bytes. It refuses a profile without a schema digest, a corpus or
// publication file that differs from its publish spec, and an embedding model
// or Catalog that disagrees with the others. Only the round's profiles and
// their publications are bound.
func BuildRoundBinding(root string, host BindingHost, round Round) ([]byte, error) {
	note := "Frozen inputs of the knowledge-product formal round (docs/kb_experiment_design.md, 正式轮规程 §2). The author approves the SHA-256 of this file; the formal gateway build seals it as -dataset-binding."
	if round.KeywordSet {
		note = "Frozen inputs of the keyword round (docs/kb_experiment_design.md, 关键词检索实验, and 正式轮规程 §2): its three profiles, their publications, and the frozen keyword set. The author approves the SHA-256 of this file; the formal gateway build seals it as -dataset-binding."
	}
	binding := Binding{Version: round.BindingVersion, Note: note}
	registry, err := os.ReadFile(filepath.Join(root, ProfilesDir("."), "registry.json"))
	if err != nil {
		return nil, err
	}
	var entries struct {
		Profiles []RegistryEntry `json:"profiles"`
	}
	if err := json.Unmarshal(registry, &entries); err != nil {
		return nil, err
	}
	if len(entries.Profiles) != len(Profiles) {
		return nil, fmt.Errorf("registry lists %d profiles, want %d", len(entries.Profiles), len(Profiles))
	}
	epochs := map[string]bool{}
	var selected []RegistryEntry
	for _, entry := range entries.Profiles {
		if round.Includes(entry.Name) {
			selected = append(selected, entry)
			epochs[entry.Epoch] = true
		}
	}
	if len(selected) != len(round.Profiles) {
		return nil, fmt.Errorf("the registry holds %d of the %d profiles of round %s", len(selected), len(round.Profiles), round.Name)
	}
	var embedding *catalog.KBEmbedding
	for _, entry := range selected {
		if entry.SchemaDigest == "" || !entry.SchemaDigestAttested {
			return nil, fmt.Errorf("profile %s has no attested schema digest", entry.Name)
		}
		binding.Profiles = append(binding.Profiles, BindingProfile{Name: entry.Name, Epoch: entry.Epoch, CatalogSHA256: entry.CatalogSHA256, SchemaDigest: entry.SchemaDigest})
		for _, name := range []string{entry.Catalog, entry.Attributes} {
			file, err := bindingFile(root, filepath.Join(ProfilesDir("."), name))
			if err != nil {
				return nil, err
			}
			binding.ProfileFiles = append(binding.ProfileFiles, file)
		}
		loaded, err := catalog.Load(filepath.Join(root, ProfilesDir("."), entry.Catalog))
		if err != nil {
			return nil, err
		}
		if loaded.SHA256 != entry.CatalogSHA256 || len(loaded.KnowledgeProducts) != 1 {
			return nil, fmt.Errorf("profile %s Catalog does not match the registry", entry.Name)
		}
		product := loaded.KnowledgeProducts[0].Embedding
		if embedding == nil {
			embedding = &product
		} else if product.Model != embedding.Model || product.Dimension != embedding.Dimension || product.WeightsSHA256 != embedding.WeightsSHA256 {
			return nil, fmt.Errorf("profile %s binds another embedding model", entry.Name)
		}
	}
	for _, name := range []string{"registry.json", "schema-attestations.json"} {
		file, err := bindingFile(root, filepath.Join(ProfilesDir("."), name))
		if err != nil {
			return nil, err
		}
		binding.ProfileFiles = append(binding.ProfileFiles, file)
	}
	sort.Slice(binding.ProfileFiles, func(i, j int) bool { return binding.ProfileFiles[i].Path < binding.ProfileFiles[j].Path })

	publicationsDir := "evaluation/kb-formal/publications"
	publicationDirs, err := os.ReadDir(filepath.Join(root, publicationsDir))
	if err != nil {
		return nil, err
	}
	corpora := map[string]*BindingCorpus{}
	for _, epochEntry := range publicationDirs {
		if !epochEntry.IsDir() || !epochs[epochEntry.Name()] {
			continue
		}
		publication := BindingPublication{Epoch: epochEntry.Name()}
		base := filepath.Join(publicationsDir, epochEntry.Name())
		err := filepath.WalkDir(filepath.Join(root, base), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			file, err := bindingFile(root, relative)
			if err != nil {
				return err
			}
			publication.Files = append(publication.Files, file)
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(publication.Files, func(i, j int) bool { return publication.Files[i].Path < publication.Files[j].Path })
		binding.Publications = append(binding.Publications, publication)
		var spec struct {
			Epoch  string `json:"epoch"`
			Corpus struct {
				Path   string `json:"path"`
				SHA256 string `json:"sha256"`
			} `json:"corpus"`
		}
		data, err := os.ReadFile(filepath.Join(root, base, "spec.json"))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &spec); err != nil {
			return nil, err
		}
		if spec.Epoch != epochEntry.Name() {
			return nil, fmt.Errorf("publication directory %s holds the spec of %s", epochEntry.Name(), spec.Epoch)
		}
		corpus := corpora[spec.Corpus.Path]
		if corpus == nil {
			documents, err := bindingFile(root, spec.Corpus.Path)
			if err != nil {
				return nil, err
			}
			manifest, err := bindingFile(root, filepath.Join(filepath.Dir(spec.Corpus.Path), "manifest.json"))
			if err != nil {
				return nil, err
			}
			corpus = &BindingCorpus{Documents: documents, Manifest: manifest}
			corpora[spec.Corpus.Path] = corpus
		}
		if corpus.Documents.SHA256 != spec.Corpus.SHA256 {
			return nil, fmt.Errorf("corpus %s differs from the spec of %s", spec.Corpus.Path, spec.Epoch)
		}
		corpus.Epochs = append(corpus.Epochs, spec.Epoch)
	}
	for _, corpus := range corpora {
		binding.Corpora = append(binding.Corpora, *corpus)
	}
	sort.Slice(binding.Corpora, func(i, j int) bool { return binding.Corpora[i].Documents.Path < binding.Corpora[j].Documents.Path })

	if embedding == nil {
		return nil, errors.New("no profile names an embedding model")
	}
	weights, err := sha256OfFile(filepath.Join(host.ModelDir, "pytorch_model.bin"))
	if err != nil {
		return nil, err
	}
	if weights != embedding.WeightsSHA256 {
		return nil, fmt.Errorf("model weights %s differ from the Catalogs' %s", weights, embedding.WeightsSHA256)
	}
	cacheDigest, cacheFiles, err := CacheContentDigest(host.CacheDir)
	if err != nil {
		return nil, err
	}
	command := exec.Command(host.VenvPython, filepath.Join(root, "tools/kb-embed/venv-identity.py"))
	var stderr bytes.Buffer
	command.Stderr = &stderr
	identityJSON, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("venv identity: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var environment map[string]any
	if err := json.Unmarshal(identityJSON, &environment); err != nil {
		return nil, err
	}
	binding.Embedding = BindingEmbedding{Model: embedding.Model, Dimension: embedding.Dimension, WeightsFile: "pytorch_model.bin", WeightsSHA256: weights,
		CacheFiles: cacheFiles, CacheContentSHA256: cacheDigest, Environment: environment}

	images, err := composeImages(filepath.Join(root, "evaluation/kb-formal/compose.kb.yaml"))
	if err != nil {
		return nil, err
	}
	for _, service := range []string{"control-postgres", "business-postgres", "result-object-store", "result-object-store-init", "kb-embed"} {
		if images[service] == "" {
			return nil, fmt.Errorf("the KB overlay does not pin the %s image by digest", service)
		}
	}
	binding.Images = images

	for _, name := range []string{"manifest.json", "queries.jsonl", "targets.json"} {
		file, err := bindingFile(root, filepath.Join("evaluation/kb-pilot/queryset-v2", name))
		if err != nil {
			return nil, err
		}
		binding.QuerySet = append(binding.QuerySet, file)
	}
	if round.KeywordSet {
		for _, name := range []string{"manifest.json", "keywords.jsonl"} {
			file, err := bindingFile(root, filepath.Join(KeywordsetDir, name))
			if err != nil {
				return nil, err
			}
			binding.KeywordSet = append(binding.KeywordSet, file)
		}
	}
	for _, relative := range []string{"tools/kb-embed/service.py", "tools/kb-embed/requirements-freeze.txt", "tools/kb-embed/venv-identity.py",
		"evaluation/kb-formal/compose.kb.yaml", "db/init-kb/00-kb-base.sh", "db/init-kb/30-kb-schema.sh", "db/init/30-kb-schema.sh",
		"db/init/kb-schema.psql", "formal-build/base-images.json"} {
		file, err := bindingFile(root, relative)
		if err != nil {
			return nil, err
		}
		binding.SourceFiles = append(binding.SourceFiles, file)
	}
	encoded, err := json.MarshalIndent(binding, "", " ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
