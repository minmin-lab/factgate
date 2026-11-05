package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// kbPlanService is a Service holding only the knowledge-product Catalog, for
// pure tests of plan construction and discovery that need no store.
func kbPlanService(t *testing.T) *Service {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := catalog.Parse([]byte(kbCatalogRewrite(t)(string(raw))))
	if err != nil {
		t.Fatalf("parse knowledge Catalog: %v", err)
	}
	return &Service{catalog: loaded}
}

// TestAggregatePlansArePinned freezes the plans aggregatePlan builds, computed
// at 1781def3 before KB1-LOOP-137 factored helpers out of it. A plan is what
// the query preparation seals, so these bytes must not move.
func TestAggregatePlansArePinned(t *testing.T) {
	declared := kbPlanService(t)
	// The test fragment admits count and sum only; a second Catalog admitting
	// all four functions pins the typed min/max columns as well.
	widened := kbPlanService(t)
	for index := range widened.catalog.KnowledgeProducts {
		widened.catalog.KnowledgeProducts[index].Retrieval.Aggregates = []string{"count", "sum", "min", "max"}
	}
	grant := control.TaskGrant{ApprovedProducts: []string{"policy_kb_values"},
		MandatoryScope: json.RawMessage(`{"kb_department":["finance"],"classification":["internal","public"]}`)}
	var transcript strings.Builder
	for _, service := range []*Service{declared, widened} {
		for _, field := range []string{"amount", "effective_on"} {
			for _, function := range []string{"count", "sum", "min", "max"} {
				for _, filters := range []string{``, `,"filters":[{"attribute":"classification","op":"in","values":["internal","public"]}]`} {
					body := `{"knowledge_product":"policy_kb","field":"` + field + `","aggregate":"` + function + `"` + filters + `}`
					request, err := retrieval.DecodeRequest(retrieval.ToolAggregate, []byte(body))
					if err != nil {
						t.Fatalf("decode %s: %v", body, err)
					}
					plan, err := service.aggregatePlan(grant, request)
					transcript.WriteString(body + "\n")
					if err != nil {
						var toolErr *mcp.ToolError
						if !errors.As(err, &toolErr) {
							t.Fatalf("%s: %T %v", body, err, err)
						}
						transcript.WriteString("error " + toolErr.Code + " " + toolErr.Message + "\n")
						continue
					}
					encoded, err := json.Marshal(plan)
					if err != nil {
						t.Fatal(err)
					}
					transcript.Write(encoded)
					transcript.WriteString("\n")
				}
			}
		}
	}
	sum := sha256.Sum256([]byte(transcript.String()))
	const want = "33d5561025cb5cba5f087c64a58d5edae93cf611efcb0923c43e7b1dd2434cad"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("aggregate plans moved: got %s want %s\n%s", got, want, transcript.String())
	}
}
