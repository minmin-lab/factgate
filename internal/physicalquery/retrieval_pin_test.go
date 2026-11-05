package physicalquery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// retrievalPinnedBytes freezes what a retrieval preparation seals, computed at
// 1781def3 before the discovery and error-detail work of KB1-LOOP-137. That
// work only reads the retrieval plan; if any of these move, a change reached
// the sealed vector, keyword or fetch preparation and must be argued on its own.
type retrievalPinnedBytes struct {
	InputsSHA256    string `json:"inputs_sha256"`
	BindingSHA256   string `json:"binding_sha256"`
	VisibleSHA256   string `json:"visible_sha256"`
	CompanionSHA256 string `json:"companion_sha256"`
}

func retrievalPinFetchInputs() RetrievalPreparationInputsV1 {
	inputs := kbTestInputs()
	inputs.NormalForm.Tool = retrieval.ToolFetch
	inputs.NormalForm.Query = nil
	inputs.NormalForm.K = 0
	inputs.NormalForm.Chunks = []retrieval.ChunkRef{{DocID: "D-9", Version: 2, ChunkIndex: 3}, {DocID: "D-1", Version: 1, ChunkIndex: 0}}
	inputs.NormalForm.Atoms = []retrieval.Atom{{Attribute: "kb_department", Type: "enum", Operator: "=", CanonicalLiteral: "s:finance", Injected: true}}
	inputs.QueryVector = nil
	return inputs
}

func retrievalPinnedOf(t *testing.T, inputs RetrievalPreparationInputsV1) retrievalPinnedBytes {
	t.Helper()
	compiler, err := LocalCompilerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareRetrievalWith(inputs, compiler)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	inputsDigest, err := inputs.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	binding, err := json.Marshal(prepared.Binding())
	if err != nil {
		t.Fatal(err)
	}
	visible, companion, err := prepared.ExecutableStatements()
	if err != nil {
		t.Fatal(err)
	}
	hash := func(value []byte) string {
		sum := sha256.Sum256(value)
		return hex.EncodeToString(sum[:])
	}
	return retrievalPinnedBytes{InputsSHA256: inputsDigest, BindingSHA256: hash(binding),
		VisibleSHA256: hash([]byte(visible)), CompanionSHA256: hash([]byte(companion))}
}

func TestRetrievalPreparationBytesArePinned(t *testing.T) {
	cases := []struct {
		name   string
		inputs func(*testing.T) RetrievalPreparationInputsV1
		want   retrievalPinnedBytes
	}{
		{name: "vector search", inputs: func(*testing.T) RetrievalPreparationInputsV1 { return kbTestInputs() },
			want: retrievalPinnedBytes{InputsSHA256: "6daed6631fcdc948748015502105ed63868f4205f5e621f106056c597ea042c7", BindingSHA256: "6cf74a756b940ee2e837d340e773e70e896817eaa7de8da634d2f4f0cf2216c2",
				VisibleSHA256: "d26f7618d65aa57da6faf1cdd5653cccaebc1cd3cf1babbcdb7daabd819deef1", CompanionSHA256: "6d09dca0c1ded7f8799418c8f50788d908fb6710aef7991b482ec454f9d66703"}},
		{name: "keyword search", inputs: func(t *testing.T) RetrievalPreparationInputsV1 { return kbTestKeywordInputs(t, "违约金", `5%_a\b`) },
			want: retrievalPinnedBytes{InputsSHA256: "32fe916af2566d183d38734bafa41c530f9cd4c7ff7d3eb8947c35eb92ac4cf4", BindingSHA256: "fc0c26d6ec6a2c85577acc003fab0528794f883cd7b337391bd319e9502c7ebf",
				VisibleSHA256: "e4845abcf6893b61e2075c7c2442f3a2a2becdddfd60afd393ebd07ff04d8433", CompanionSHA256: "edaea472f4602d59521d52a8da26bc200e600da46e6a04d180f0a31530ed0263"}},
		{name: "fetch", inputs: func(*testing.T) RetrievalPreparationInputsV1 { return retrievalPinFetchInputs() },
			want: retrievalPinnedBytes{InputsSHA256: "2261a708ef0acac0fb78ab270505dc5ec049bc255d69956aeab3fa305419ac19", BindingSHA256: "c5a1c39db6b440d655acf0cfdc449df9c4700014efe53782f8d6c53947a0d511",
				VisibleSHA256: "f789a9d04daaa6818d7816884b4c2370fb6825f374305747f44cb02d19535e4d", CompanionSHA256: "5e26fcffec3580b11bae9ce0dad056e365501afbabab78c931a27454b30204ba"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := retrievalPinnedOf(t, tc.inputs(t))
			if got != tc.want {
				t.Fatalf("sealed retrieval bytes moved:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}
