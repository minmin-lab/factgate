package sqlpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// Engine is immutable and safe for concurrent use.
type Engine struct {
	defaultFunctions  map[string]struct{}
	defaultAggregates map[string]struct{}
	defaultOperators  map[string]struct{}
}

// New constructs a PostgreSQL AST policy engine.
func New(config Config) *Engine {
	functions := config.AllowedFunctions
	if functions == nil {
		functions = defaultFunctions
	}
	aggregates := config.AllowedAggregates
	if aggregates == nil {
		if config.AllowedFunctions != nil {
			aggregates = config.AllowedFunctions
		} else {
			aggregates = defaultAggregates
		}
	}
	operators := config.AllowedOperators
	if operators == nil {
		operators = defaultOperators
	}
	return &Engine{
		defaultFunctions:  stringSet(functions),
		defaultAggregates: stringSet(aggregates),
		defaultOperators:  operatorSet(operators),
	}
}

// Authorize validates SQL exclusively through pg_query_go's PostgreSQL AST.
// It never uses textual pattern matching to decide whether agent SQL is safe.
func (e *Engine) Authorize(request Request) (Decision, error) {
	if request.RowLimit <= 0 {
		return Decision{}, reject(CodeBudgetExhausted)
	}
	products, err := e.validateGrant(request.Grant)
	if err != nil {
		return Decision{}, err
	}

	astJSON, parseErr := pg_query.ParseToJSON(request.SQL)
	if parseErr != nil {
		return Decision{}, reject(CodeInvalidSQL)
	}
	var document any
	decoder := json.NewDecoder(strings.NewReader(astJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return Decision{}, reject(CodeInvalidSQL)
	}

	root, ok := document.(map[string]any)
	if !ok {
		return Decision{}, reject(CodeInvalidSQL)
	}
	statements, ok := root["stmts"].([]any)
	if !ok || len(statements) == 0 {
		return Decision{}, reject(CodeInvalidSQL)
	}
	if len(statements) != 1 {
		return Decision{}, reject(CodeMultipleStatements)
	}
	statement, ok := statements[0].(map[string]any)
	if !ok {
		return Decision{}, reject(CodeInvalidSQL)
	}
	stmt, ok := statement["stmt"].(map[string]any)
	if !ok || len(stmt) != 1 {
		return Decision{}, reject(CodeInvalidSQL)
	}
	selectBody, ok := stmt["SelectStmt"].(map[string]any)
	if !ok {
		return Decision{}, reject(topLevelCode(stmt))
	}

	analyzer := newAnalyzer(products, e.defaultFunctions, e.defaultAggregates, e.defaultOperators)
	if _, err := analyzer.analyzeSelect(selectBody, nil, nil); err != nil {
		return Decision{}, err
	}

	parsed, err := pg_query.Parse(request.SQL)
	if err != nil || len(parsed.GetStmts()) != 1 {
		return Decision{}, reject(CodeInvalidSQL)
	}
	canonical, err := pg_query.Deparse(parsed)
	if err != nil {
		return Decision{}, reject(CodeInvalidSQL)
	}
	canonical = strings.TrimSpace(canonical)
	canonical = strings.TrimSuffix(canonical, ";")
	if canonical == "" {
		return Decision{}, reject(CodeInvalidSQL)
	}

	referencedProducts := sortedKeys(analyzer.referencedProducts)
	referencedColumns := sortedKeys(analyzer.referencedColumns)
	executable, err := renderExecutable(canonical, referencedProducts, analyzer.renderProducts(), request.RowLimit)
	if err != nil {
		return Decision{}, err
	}
	digest := sha256.Sum256([]byte(canonical))
	return Decision{
		SQL:                executable,
		CanonicalAgentSQL:  canonical,
		Fingerprint:        hex.EncodeToString(digest[:]),
		ReferencedProducts: referencedProducts,
		ReferencedColumns:  referencedColumns,
		RowLimit:           request.RowLimit,
	}, nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

// operatorAliases maps the Catalog's word spellings of the LIKE family to the
// operator names PostgreSQL's AST carries, which operatorAllowed looks up
// (LIKE parses to ~~). It renames only: declaring like admits ~~ and nothing
// more, so ILIKE and NOT LIKE stay separately declared.
var operatorAliases = map[string]string{
	"like": "~~", "ilike": "~~*", "not like": "!~~", "not ilike": "!~~*",
}

// CanonicalOperator renames a LIKE-family word to its AST symbol and returns
// any other name unchanged. OperatorKey applies it after normalizing case and
// space.
func CanonicalOperator(operator string) string {
	if canonical, ok := operatorAliases[strings.ToLower(strings.TrimSpace(operator))]; ok {
		return canonical
	}
	return operator
}

// OperatorKey returns the key a declared operator is authorized under, the
// name operatorKeys derives from the AST: surrounding space is trimmed, a word
// (a name holding a letter) is lowercased, and a LIKE-family word becomes its
// symbol. A symbol comes back trimmed and otherwise unchanged; PostgreSQL
// operator symbols hold no letters. Discovery calls it so what it lists is
// what Authorize admits.
func OperatorKey(operator string) string {
	key := strings.TrimSpace(operator)
	if strings.IndexFunc(key, unicode.IsLetter) >= 0 {
		key = strings.ToLower(key)
	}
	return CanonicalOperator(key)
}

func operatorSet(operators []string) map[string]struct{} {
	keys := make([]string, 0, len(operators))
	for _, operator := range operators {
		keys = append(keys, OperatorKey(operator))
	}
	return stringSet(keys)
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func topLevelCode(stmt map[string]any) Code {
	for node := range stmt {
		if _, found := writeNodes[node]; found {
			return CodeWriteForbidden
		}
	}
	return CodeNotSelect
}

func (e *Engine) validateGrant(grant Grant) (map[string]productPolicy, error) {
	products := make(map[string]productPolicy, len(grant.Products))
	for _, product := range grant.Products {
		if !safeCatalogIdentifier(product.LogicalName) || product.PhysicalSchema == "" || product.PhysicalView == "" || len(product.ApprovedColumns) == 0 {
			return nil, reject(CodeInvalidGrant)
		}
		if _, duplicate := products[product.LogicalName]; duplicate {
			return nil, reject(CodeInvalidGrant)
		}
		seenColumns := make(map[string]struct{}, len(product.ApprovedColumns))
		for _, column := range product.ApprovedColumns {
			if !safeCatalogIdentifier(column) {
				return nil, reject(CodeInvalidGrant)
			}
			if _, duplicate := seenColumns[column]; duplicate {
				return nil, reject(CodeInvalidGrant)
			}
			seenColumns[column] = struct{}{}
		}
		for _, predicate := range product.MandatoryScope {
			if !safeCatalogIdentifier(predicate.Column) || !validPredicateShape(predicate) {
				return nil, reject(CodeInvalidGrant)
			}
			for _, value := range predicate.Values {
				if strings.IndexByte(value, 0) >= 0 {
					return nil, reject(CodeInvalidGrant)
				}
			}
		}
		if strings.IndexByte(product.PhysicalSchema, 0) >= 0 || strings.IndexByte(product.PhysicalView, 0) >= 0 {
			return nil, reject(CodeInvalidGrant)
		}
		policy, err := e.productPolicy(product)
		if err != nil {
			return nil, err
		}
		products[product.LogicalName] = policy
	}
	return products, nil
}

func (e *Engine) productPolicy(product ProductGrant) (productPolicy, error) {
	functions := product.AllowedFunctions
	if functions == nil {
		functions = sortedKeys(e.defaultFunctions)
	}
	aggregates := product.AllowedAggregates
	if aggregates == nil {
		if product.AllowedFunctions != nil {
			aggregates = product.AllowedFunctions
		} else {
			aggregates = sortedKeys(e.defaultAggregates)
		}
	}
	operators := product.AllowedOperators
	if operators == nil {
		operators = sortedKeys(e.defaultOperators)
	}
	for _, function := range append(append([]string(nil), functions...), aggregates...) {
		if function == "" {
			return productPolicy{}, reject(CodeInvalidGrant)
		}
	}
	for _, operator := range operators {
		if operator == "" {
			return productPolicy{}, reject(CodeInvalidGrant)
		}
	}
	return productPolicy{
		grant:      product,
		columns:    stringSet(product.ApprovedColumns),
		functions:  stringSet(functions),
		aggregates: stringSet(aggregates),
		operators:  operatorSet(operators),
	}, nil
}

func safeCatalogIdentifier(value string) bool {
	if value == "" || strings.HasPrefix(value, "__taskbound_") {
		return false
	}
	for index, r := range value {
		if r == '_' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

var defaultFunctions = []string{
	"abs", "btrim", "ceil", "ceiling", "char_length", "concat",
	"concat_ws", "date_part", "date_trunc", "floor", "jsonb_array_length",
	"jsonb_extract_path", "jsonb_extract_path_text", "length", "lower", "ltrim",
	"replace", "round", "rtrim", "substring", "to_char", "trim", "upper",
}

var defaultAggregates = []string{
	"avg", "count", "max", "min", "sum",
}

var defaultOperators = []string{
	"=", "<>", "!=", "<", "<=", ">", ">=", "+", "-", "*", "/", "%",
	"||", "~~", "!~~", "~~*", "!~~*", "->", "->>", "#>", "#>>", "@>", "<@",
}
