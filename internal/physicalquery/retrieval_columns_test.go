package physicalquery

import (
	"reflect"
	"testing"

	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

func TestRetrievalRequiredColumnsAreThePlanColumns(t *testing.T) {
	kp := kbTestKnowledgeProduct()
	base, err := RetrievalRequiredColumns(kp)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		testKBChunks:    {"chunk_index", "doc_id", "text_sha256", "version"},
		testKBDocuments: {"doc_id", "version"},
	}
	if !reflect.DeepEqual(base, want) {
		t.Fatalf("base retrieval columns %v, want %v", base, want)
	}
	filtered, err := RetrievalRequiredColumns(kp, "job_role", "valid_until")
	if err != nil {
		t.Fatal(err)
	}
	if got := filtered[testKBChunks]; !reflect.DeepEqual(got, []string{"chunk_index", "doc_id", "job_role", "text_sha256", "valid_until", "version"}) {
		t.Fatalf("filtered chunk columns %v", got)
	}
	if _, err := RetrievalRequiredColumns(kp, "colour"); err == nil {
		t.Fatal("an undeclared attribute produced columns")
	}
	// The fixture normal form: two caller attributes and one injected scope
	// atom, which the policy renderer applies and the plan does not read.
	planned, err := RetrievalPlanColumns(kp, kbTestInputs().NormalForm)
	if err != nil {
		t.Fatal(err)
	}
	if got := planned[testKBChunks]; !reflect.DeepEqual(got, []string{"chunk_index", "classification", "doc_id", "job_role", "text_sha256", "version"}) {
		t.Fatalf("fixture plan chunk columns %v; the injected kb_department atom must add none", got)
	}
	if RetrievalOperatorRendered("has") || !RetrievalOperatorRendered("in") {
		t.Fatal("operator rendering disagrees with the plan's operator table")
	}
}

// TestRetrievalRequiredColumnsAreSufficientAndMinimal proves the derived set
// is exactly what preparation needs: a grant of only those columns prepares
// every retrieval shape and passes the policy gate, and withholding any one
// of them refuses the preparation.
func TestRetrievalRequiredColumnsAreSufficientAndMinimal(t *testing.T) {
	compiler, err := LocalCompilerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	shapes := map[string]RetrievalPreparationInputsV1{
		"vector search":  kbTestInputs(),
		"keyword search": kbTestKeywordInputs(t, "违约金"),
		"fetch":          retrievalPinFetchInputs(),
	}
	for name, inputs := range shapes {
		t.Run(name, func(t *testing.T) {
			required, err := RetrievalPlanColumns(inputs.KnowledgeProduct, inputs.NormalForm)
			if err != nil {
				t.Fatal(err)
			}
			exact := inputs
			exact.Grant.ApprovedColumns = required
			prepared, err := PrepareRetrievalWith(exact, compiler)
			if err != nil {
				t.Fatalf("a grant of exactly the required columns %v did not prepare: %v", required, err)
			}
			visible, companion, err := prepared.ExecutableStatements()
			if err != nil {
				t.Fatal(err)
			}
			engine := sqlpolicy.New(sqlpolicy.Config{})
			for _, statement := range []string{visible, companion} {
				if _, err := engine.Authorize(sqlpolicy.Request{SQL: statement, Grant: prepared.PolicyGrant(), RowLimit: 20}); err != nil {
					t.Fatalf("policy rejected a statement prepared from the required columns: %v", err)
				}
			}
			for relation, columns := range required {
				for skip := range columns {
					narrowed := exact
					narrowed.Grant.ApprovedColumns = map[string][]string{}
					for other, otherColumns := range required {
						narrowed.Grant.ApprovedColumns[other] = append([]string(nil), otherColumns...)
					}
					kept := append(append([]string(nil), columns[:skip]...), columns[skip+1:]...)
					narrowed.Grant.ApprovedColumns[relation] = kept
					if _, err := PrepareRetrievalWith(narrowed, compiler); err == nil {
						t.Fatalf("withholding %s.%s still prepared; the required set is not minimal", relation, columns[skip])
					}
				}
			}
		})
	}
}
