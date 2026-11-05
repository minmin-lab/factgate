package sqlpolicy

import (
	"sort"
	"strings"
)

type productPolicy struct {
	grant      ProductGrant
	columns    map[string]struct{}
	functions  map[string]struct{}
	aggregates map[string]struct{}
	operators  map[string]struct{}
}

type productSet map[string]struct{}

type columnBinding struct {
	Name       string
	Sources    productSet
	References map[string]struct{}
	Ambiguous  bool
}

type relationBinding struct {
	Name    string
	Columns map[string]columnBinding
	Order   []columnBinding
}

type selectResult struct {
	Columns []columnBinding
	Sources productSet
}

type analyzer struct {
	products            map[string]productPolicy
	taskGlobalFunctions map[string]struct{}
	taskGlobalOperators map[string]struct{}
	referencedProducts  map[string]struct{}
	referencedColumns   map[string]struct{}
}

type scope struct {
	analyzer      *analyzer
	parent        *scope
	relations     map[string]relationBinding
	ctes          map[string]relationBinding
	outputAliases map[string]columnBinding
}

func newAnalyzer(products map[string]productPolicy, functions, _ map[string]struct{}, operators map[string]struct{}) *analyzer {
	return &analyzer{
		products:            products,
		taskGlobalFunctions: functions,
		taskGlobalOperators: operators,
		referencedProducts:  make(map[string]struct{}),
		referencedColumns:   make(map[string]struct{}),
	}
}

func (a *analyzer) renderProducts() map[string]ProductGrant {
	result := make(map[string]ProductGrant, len(a.products))
	for name, product := range a.products {
		result[name] = product.grant
	}
	return result
}

func (a *analyzer) analyzeSelect(body map[string]any, parent *scope, inheritedCTEs map[string]relationBinding) (selectResult, error) {
	if err := validateSelectBasics(body); err != nil {
		return selectResult{}, err
	}
	if hasSetOperation(body) {
		return a.analyzeSetOperation(body, parent, inheritedCTEs)
	}

	current := &scope{
		analyzer:  a,
		parent:    parent,
		relations: make(map[string]relationBinding),
		ctes:      cloneRelations(inheritedCTEs),
	}
	if err := a.processWithClause(current, body["withClause"]); err != nil {
		return selectResult{}, err
	}
	if err := a.processFromClause(current, body["fromClause"]); err != nil {
		return selectResult{}, err
	}

	sources := make(productSet)
	for _, key := range []string{"whereClause", "groupClause", "havingClause", "distinctClause"} {
		binding, err := a.validateExpr(body[key], current, false)
		if err != nil {
			return selectResult{}, err
		}
		mergeInto(sources, binding.Sources)
	}

	output, targetSources, err := a.analyzeTargetList(current, body["targetList"])
	if err != nil {
		return selectResult{}, err
	}
	mergeInto(sources, targetSources)
	current.outputAliases = outputAliasMap(output)

	for _, key := range []string{"sortClause"} {
		binding, err := a.validateExpr(body[key], current, true)
		if err != nil {
			return selectResult{}, err
		}
		mergeInto(sources, binding.Sources)
	}
	for _, key := range []string{"limitCount", "limitOffset", "windowClause"} {
		binding, err := a.validateExpr(body[key], current, false)
		if err != nil {
			return selectResult{}, err
		}
		mergeInto(sources, binding.Sources)
	}

	if err := a.validateUnhandledSelectFields(body, current); err != nil {
		return selectResult{}, err
	}
	return selectResult{Columns: output, Sources: sources}, nil
}

func validateSelectBasics(body map[string]any) error {
	if _, present := body["intoClause"]; present {
		return reject(CodeSelectInto)
	}
	if clauses, present := body["lockingClause"].([]any); present && len(clauses) != 0 {
		return reject(CodeLocking)
	}
	if values, present := body["valuesLists"].([]any); present && len(values) != 0 {
		return reject(CodeFeatureNotAllowed)
	}
	if with, present := body["withClause"].(map[string]any); present {
		if recursive, _ := with["recursive"].(bool); recursive {
			return reject(CodeRecursiveCTE)
		}
	}
	return nil
}

func hasSetOperation(body map[string]any) bool {
	if _, ok := body["larg"]; ok {
		return true
	}
	if _, ok := body["rarg"]; ok {
		return true
	}
	if op, ok := body["op"].(string); ok && op != "" && op != "SETOP_NONE" {
		return true
	}
	return false
}

func (a *analyzer) analyzeSetOperation(body map[string]any, parent *scope, inheritedCTEs map[string]relationBinding) (selectResult, error) {
	ctes := cloneRelations(inheritedCTEs)
	if _, present := body["withClause"]; present {
		current := &scope{
			analyzer:  a,
			parent:    parent,
			relations: make(map[string]relationBinding),
			ctes:      ctes,
		}
		if err := a.processWithClause(current, body["withClause"]); err != nil {
			return selectResult{}, err
		}
		ctes = current.ctes
	}
	leftBody, err := selectStmtBody(body["larg"])
	if err != nil {
		return selectResult{}, err
	}
	rightBody, err := selectStmtBody(body["rarg"])
	if err != nil {
		return selectResult{}, err
	}
	left, err := a.analyzeSelect(leftBody, parent, ctes)
	if err != nil {
		return selectResult{}, err
	}
	right, err := a.analyzeSelect(rightBody, parent, ctes)
	if err != nil {
		return selectResult{}, err
	}
	if len(left.Columns) != len(right.Columns) {
		return selectResult{}, reject(CodeColumnNotAllowed)
	}
	output := make([]columnBinding, len(left.Columns))
	sources := copyProductSet(left.Sources)
	mergeInto(sources, right.Sources)
	for index := range left.Columns {
		output[index] = mergeColumns(left.Columns[index], right.Columns[index])
		if output[index].Name == "" {
			output[index].Name = left.Columns[index].Name
		}
	}
	return selectResult{Columns: output, Sources: sources}, nil
}

func (a *analyzer) processWithClause(current *scope, value any) error {
	with, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if recursive, _ := with["recursive"].(bool); recursive {
		return reject(CodeRecursiveCTE)
	}
	items, _ := with["ctes"].([]any)
	for _, item := range items {
		name, body, ok := unwrapNode(item)
		if !ok || name != "CommonTableExpr" {
			return reject(CodeInvalidSQL)
		}
		if recursive, _ := body["cterecursive"].(bool); recursive {
			return reject(CodeRecursiveCTE)
		}
		cteName, _ := body["ctename"].(string)
		if !safeCatalogIdentifier(cteName) {
			return reject(CodeInvalidSQL)
		}
		queryBody, err := selectStmtBody(body["ctequery"])
		if err != nil {
			return err
		}
		cteScope := &scope{
			analyzer:  a,
			relations: make(map[string]relationBinding),
			ctes:      cloneRelations(current.ctes),
		}
		result, err := a.analyzeSelect(queryBody, cteScope.parent, cteScope.ctes)
		if err != nil {
			return err
		}
		relation, err := relationFromOutputs(cteName, result.Columns)
		if err != nil {
			return err
		}
		relation, err = applyAliasColumnNames(relation, nodeStringList(body["aliascolnames"]))
		if err != nil {
			return err
		}
		current.ctes[cteName] = relation
	}
	return nil
}

func (a *analyzer) processFromClause(current *scope, value any) error {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	for _, item := range items {
		if err := a.addFromItem(current, item); err != nil {
			return err
		}
	}
	return nil
}

func (a *analyzer) addFromItem(current *scope, item any) error {
	name, body, ok := unwrapNode(item)
	if !ok {
		return reject(CodeInvalidSQL)
	}
	switch name {
	case "RangeVar":
		relation, err := a.rangeVarRelation(current, body)
		if err != nil {
			return err
		}
		aliasName, aliasColumns, err := parseAlias(body["alias"])
		if err != nil {
			return err
		}
		if aliasName != "" {
			relation.Name = aliasName
		}
		relation, err = applyAliasColumnNames(relation, aliasColumns)
		if err != nil {
			return err
		}
		return current.addRelation(relation)
	case "RangeSubselect":
		aliasName, aliasColumns, err := parseAlias(body["alias"])
		if err != nil {
			return err
		}
		if aliasName == "" {
			return reject(CodeObjectNotAllowed)
		}
		queryBody, err := selectStmtBody(body["subquery"])
		if err != nil {
			return err
		}
		var parent *scope
		if lateral, _ := body["lateral"].(bool); lateral {
			parent = current
		}
		result, err := a.analyzeSelect(queryBody, parent, current.ctes)
		if err != nil {
			return err
		}
		relation, err := relationFromOutputs(aliasName, result.Columns)
		if err != nil {
			return err
		}
		relation, err = applyAliasColumnNames(relation, aliasColumns)
		if err != nil {
			return err
		}
		return current.addRelation(relation)
	case "JoinExpr":
		return a.addJoin(current, body)
	default:
		if code, denied := deniedNodes[name]; denied {
			return reject(code)
		}
		return reject(CodeFeatureNotAllowed)
	}
}

func (a *analyzer) rangeVarRelation(current *scope, body map[string]any) (relationBinding, error) {
	if schema, _ := body["schemaname"].(string); schema != "" {
		return relationBinding{}, reject(CodeSystemObject)
	}
	if catalog, _ := body["catalogname"].(string); catalog != "" {
		return relationBinding{}, reject(CodeSystemObject)
	}
	relationName, _ := body["relname"].(string)
	if relationName == "" {
		return relationBinding{}, reject(CodeObjectNotAllowed)
	}
	if systemRelation(relationName) {
		return relationBinding{}, reject(CodeSystemObject)
	}
	if cte, ok := current.ctes[relationName]; ok {
		cte.Name = relationName
		return cte, nil
	}
	product, ok := a.products[relationName]
	if !ok {
		return relationBinding{}, reject(CodeObjectNotAllowed)
	}
	a.referencedProducts[relationName] = struct{}{}
	return productRelation(product), nil
}

func (a *analyzer) addJoin(current *scope, body map[string]any) error {
	before := relationNames(current.relations)
	if err := a.addFromItem(current, body["larg"]); err != nil {
		return err
	}
	if err := a.addFromItem(current, body["rarg"]); err != nil {
		return err
	}
	if _, err := a.validateExpr(body["quals"], current, false); err != nil {
		return err
	}
	aliasName, aliasColumns, err := parseAlias(body["alias"])
	if err != nil {
		return err
	}
	if aliasName == "" {
		return nil
	}
	joined := current.relationsAddedSince(before)
	for name := range joined {
		delete(current.relations, name)
	}
	relation, err := combineJoinRelations(aliasName, joined)
	if err != nil {
		return err
	}
	relation, err = applyAliasColumnNames(relation, aliasColumns)
	if err != nil {
		return err
	}
	return current.addRelation(relation)
}

func (a *analyzer) analyzeTargetList(current *scope, value any) ([]columnBinding, productSet, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, nil, reject(CodeFeatureNotAllowed)
	}
	output := make([]columnBinding, 0, len(items))
	sources := make(productSet)
	for _, item := range items {
		name, body, ok := unwrapNode(item)
		if !ok || name != "ResTarget" {
			return nil, nil, reject(CodeInvalidSQL)
		}
		alias, _ := body["name"].(string)
		if alias != "" && !safeCatalogIdentifier(alias) {
			return nil, nil, reject(CodeColumnNotAllowed)
		}
		expr, err := a.validateExpr(body["val"], current, false)
		if err != nil {
			return nil, nil, err
		}
		mergeInto(sources, expr.Sources)
		if alias == "" {
			alias = outputName(body["val"])
		}
		expr.Name = alias
		output = append(output, expr)
	}
	return output, sources, nil
}

func (a *analyzer) validateUnhandledSelectFields(body map[string]any, current *scope) error {
	handled := map[string]struct{}{
		"all": {}, "distinctClause": {}, "fromClause": {}, "groupClause": {}, "havingClause": {},
		"intoClause": {}, "larg": {}, "limitCount": {}, "limitOffset": {}, "lockingClause": {},
		"op": {}, "rarg": {}, "sortClause": {}, "targetList": {}, "valuesLists": {},
		"whereClause": {}, "windowClause": {}, "withClause": {},
	}
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := handled[key]; ok {
			continue
		}
		if _, err := a.validateExpr(body[key], current, false); err != nil {
			return err
		}
	}
	return nil
}

func (a *analyzer) validateExpr(value any, current *scope, allowOutput bool) (columnBinding, error) {
	switch typed := value.(type) {
	case nil:
		return emptyBinding(), nil
	case []any:
		result := emptyBinding()
		for _, item := range typed {
			child, err := a.validateExpr(item, current, allowOutput)
			if err != nil {
				return columnBinding{}, err
			}
			result = mergeColumns(result, child)
		}
		return result, nil
	case map[string]any:
		if name, body, ok := unwrapNode(typed); ok {
			if code, denied := deniedNodes[name]; denied {
				return columnBinding{}, reject(code)
			}
			if _, allowed := allowedNodes[name]; !allowed {
				return columnBinding{}, reject(CodeFeatureNotAllowed)
			}
			switch name {
			case "ColumnRef":
				return a.validateColumnRef(current, body, allowOutput)
			case "FuncCall":
				return a.validateFunction(current, body, allowOutput)
			case "A_Expr":
				return a.validateOperator(current, body, allowOutput)
			case "SubLink":
				return a.validateSubLink(current, body, allowOutput, false)
			case "BoolExpr":
				if sublink, ok := negatedInSubquery(body); ok {
					return a.validateSubLink(current, sublink, allowOutput, true)
				}
				return a.validateExprMap(body, current, allowOutput)
			case "TypeCast":
				if err := validateTypeCast(body); err != nil {
					return columnBinding{}, err
				}
				return a.validateExpr(body["arg"], current, allowOutput)
			case "SelectStmt":
				result, err := a.analyzeSelect(body, current, current.ctes)
				if err != nil {
					return columnBinding{}, err
				}
				return bindingFromResult(result), nil
			case "RangeVar", "RangeSubselect", "JoinExpr", "CommonTableExpr":
				return columnBinding{}, reject(CodeFeatureNotAllowed)
			default:
				return a.validateExprMap(body, current, allowOutput)
			}
		}
		return a.validateExprMap(typed, current, allowOutput)
	default:
		return emptyBinding(), nil
	}
}

func (a *analyzer) validateExprMap(body map[string]any, current *scope, allowOutput bool) (columnBinding, error) {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := emptyBinding()
	for _, key := range keys {
		if key == "location" {
			continue
		}
		child, err := a.validateExpr(body[key], current, allowOutput)
		if err != nil {
			return columnBinding{}, err
		}
		result = mergeColumns(result, child)
	}
	return result, nil
}

func (a *analyzer) validateColumnRef(current *scope, body map[string]any, allowOutput bool) (columnBinding, error) {
	parts, wildcard, valid := nodeIdentifierPath(body["fields"])
	if wildcard {
		return columnBinding{}, reject(CodeWildcard)
	}
	if !valid || len(parts) == 0 || len(parts) > 2 {
		return columnBinding{}, reject(CodeColumnNotAllowed)
	}
	binding, err := current.resolveColumn(parts, allowOutput)
	if err != nil {
		return columnBinding{}, err
	}
	if binding.Ambiguous {
		return columnBinding{}, reject(CodeColumnNotAllowed)
	}
	for reference := range binding.References {
		a.referencedColumns[reference] = struct{}{}
	}
	return binding, nil
}

func (a *analyzer) validateFunction(current *scope, body map[string]any, allowOutput bool) (columnBinding, error) {
	nameParts := nodeStringList(body["funcname"])
	if len(nameParts) != 1 {
		return columnBinding{}, reject(CodeFunctionNotAllowed)
	}
	name := nameParts[0]
	if dangerousFunction(name) {
		return columnBinding{}, reject(CodeFunctionNotAllowed)
	}
	args, err := a.functionArgumentSources(current, body, allowOutput)
	if err != nil {
		return columnBinding{}, err
	}
	sources := copyProductSet(args.Sources)
	if isAggregateStar(body) && len(sources) == 0 {
		sources = current.localVisibleProducts()
	}
	if !a.functionAllowed(name, sources) {
		return columnBinding{}, reject(CodeFunctionNotAllowed)
	}
	args.Sources = sources
	return args, nil
}

func (a *analyzer) functionArgumentSources(current *scope, body map[string]any, allowOutput bool) (columnBinding, error) {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := emptyBinding()
	for _, key := range keys {
		switch key {
		case "funcname", "location", "agg_star", "agg_distinct", "func_variadic", "funcformat":
			continue
		}
		child, err := a.validateExpr(body[key], current, allowOutput)
		if err != nil {
			return columnBinding{}, err
		}
		result = mergeColumns(result, child)
	}
	return result, nil
}

func (a *analyzer) functionAllowed(name string, sources productSet) bool {
	if len(sources) == 0 {
		_, ok := a.taskGlobalFunctions[name]
		return ok
	}
	for productName := range sources {
		product, ok := a.products[productName]
		if !ok {
			return false
		}
		if _, ok := product.functions[name]; ok {
			continue
		}
		if _, ok := product.aggregates[name]; ok {
			continue
		}
		return false
	}
	return true
}

func (a *analyzer) validateOperator(current *scope, body map[string]any, allowOutput bool) (columnBinding, error) {
	operands := emptyBinding()
	for _, key := range []string{"lexpr", "rexpr"} {
		child, err := a.validateExpr(body[key], current, allowOutput)
		if err != nil {
			return columnBinding{}, err
		}
		operands = mergeColumns(operands, child)
	}
	operators := nodeStringList(body["name"])
	if len(operators) != 1 {
		return columnBinding{}, reject(CodeOperatorNotAllowed)
	}
	kind, _ := body["kind"].(string)
	if !a.operatorAllowed(operatorKeys(kind, operators[0]), operands.Sources) {
		return columnBinding{}, reject(CodeOperatorNotAllowed)
	}
	return operands, nil
}

// validateSubLink validates a subquery predicate's operands as validateExpr
// would (the subselect is a SelectStmt and goes through analyzeSelect) and
// gates the comparison of the ANY, ALL and ROWCOMPARE forms as validateOperator
// gates an A_Expr. pg_query names no operator for x IN (SELECT ...), whose
// implied operator is =, so it is looked up under = or in, and its negation
// (negated, see negatedInSubquery) under <> or not in. An explicit op ANY or op
// ALL (SELECT ...) is looked up by op alone, as op ANY (ARRAY ...) is. EXISTS,
// scalar, ARRAY, MULTIEXPR and CTE sublinks are not operators: an operator
// applied to one is an A_Expr gated on its own, and the subquery is bound by
// its own column, product and operator checks.
func (a *analyzer) validateSubLink(current *scope, body map[string]any, allowOutput, negated bool) (columnBinding, error) {
	operands := emptyBinding()
	for _, key := range []string{"testexpr", "subselect"} {
		child, err := a.validateExpr(body[key], current, allowOutput)
		if err != nil {
			return columnBinding{}, err
		}
		operands = mergeColumns(operands, child)
	}
	kind, _ := body["subLinkType"].(string)
	switch kind {
	case "ANY_SUBLINK", "ALL_SUBLINK", "ROWCOMPARE_SUBLINK":
	default:
		return operands, nil
	}
	var keys []string
	if _, named := body["operName"]; !named {
		keys = []string{"=", "in"}
		if negated {
			keys = []string{"<>", "not in"}
		}
	} else {
		operators := nodeStringList(body["operName"])
		if len(operators) != 1 {
			return columnBinding{}, reject(CodeOperatorNotAllowed)
		}
		keys = []string{operators[0]}
	}
	if !a.operatorAllowed(keys, operands.Sources) {
		return columnBinding{}, reject(CodeOperatorNotAllowed)
	}
	return operands, nil
}

// negatedInSubquery returns the SubLink of x NOT IN (SELECT ...), which
// pg_query parses as a NOT BoolExpr whose single argument is an ANY SubLink
// with no operator name. NOT (x IN (SELECT ...)) is the same tree, so both
// spellings are gated as not in.
func negatedInSubquery(body map[string]any) (map[string]any, bool) {
	if boolop, _ := body["boolop"].(string); boolop != "NOT_EXPR" {
		return nil, false
	}
	args, ok := body["args"].([]any)
	if !ok || len(args) != 1 {
		return nil, false
	}
	name, sublink, ok := unwrapNode(args[0])
	if !ok || name != "SubLink" {
		return nil, false
	}
	if kind, _ := sublink["subLinkType"].(string); kind != "ANY_SUBLINK" {
		return nil, false
	}
	if _, named := sublink["operName"]; named {
		return nil, false
	}
	return sublink, true
}

// operatorKeys returns the keys an A_Expr is authorized under; declaring any
// one of them admits it. pg_query names an IN list by its element operator (=
// for IN, <> for NOT IN), so = and <> keep admitting IN and NOT IN, and in and
// not in admit the list alone, not a scalar comparison, IS DISTINCT FROM or =
// ANY. The BETWEEN family carries uppercase words and is looked up by their
// lowercase, so between admits neither NOT BETWEEN nor BETWEEN SYMMETRIC. Any
// other kind, or none, is looked up by its operator name alone.
func operatorKeys(kind, name string) []string {
	switch kind {
	case "AEXPR_IN":
		switch name {
		case "=":
			return []string{name, "in"}
		case "<>":
			return []string{name, "not in"}
		}
	case "AEXPR_BETWEEN", "AEXPR_NOT_BETWEEN", "AEXPR_BETWEEN_SYM", "AEXPR_NOT_BETWEEN_SYM":
		return []string{strings.ToLower(name)}
	}
	return []string{name}
}

// operatorAllowed reports whether every source product declares at least one
// of keys, or, for an expression over no product, whether the task-global set
// holds one.
func (a *analyzer) operatorAllowed(keys []string, sources productSet) bool {
	if len(sources) == 0 {
		return holdsAny(a.taskGlobalOperators, keys)
	}
	for productName := range sources {
		product, ok := a.products[productName]
		if !ok {
			return false
		}
		if !holdsAny(product.operators, keys) {
			return false
		}
	}
	return true
}

func holdsAny(set map[string]struct{}, keys []string) bool {
	for _, key := range keys {
		if _, ok := set[key]; ok {
			return true
		}
	}
	return false
}

func (s *scope) addRelation(relation relationBinding) error {
	if relation.Name == "" || !safeCatalogIdentifier(relation.Name) {
		return reject(CodeObjectNotAllowed)
	}
	if _, exists := s.relations[relation.Name]; exists {
		return reject(CodeObjectNotAllowed)
	}
	s.relations[relation.Name] = relation
	return nil
}

func (s *scope) resolveColumn(parts []string, allowOutput bool) (columnBinding, error) {
	if len(parts) == 2 {
		relation, ok := s.lookupRelation(parts[0])
		if !ok {
			return columnBinding{}, reject(CodeObjectNotAllowed)
		}
		column, ok := relation.Columns[parts[1]]
		if !ok || column.Ambiguous {
			return columnBinding{}, reject(CodeColumnNotAllowed)
		}
		return column, nil
	}
	name := parts[0]
	if allowOutput && s.outputAliases != nil {
		if column, ok := s.outputAliases[name]; ok {
			if column.Ambiguous {
				return columnBinding{}, reject(CodeColumnNotAllowed)
			}
			return column, nil
		}
	}
	var match columnBinding
	matches := 0
	for _, relation := range s.relations {
		column, ok := relation.Columns[name]
		if !ok {
			continue
		}
		if column.Ambiguous {
			return columnBinding{}, reject(CodeColumnNotAllowed)
		}
		match = column
		matches++
	}
	if matches > 1 {
		return columnBinding{}, reject(CodeColumnNotAllowed)
	}
	if matches == 1 {
		return match, nil
	}
	if s.parent != nil {
		return s.parent.resolveColumn(parts, false)
	}
	return columnBinding{}, reject(CodeColumnNotAllowed)
}

func (s *scope) lookupRelation(name string) (relationBinding, bool) {
	if relation, ok := s.relations[name]; ok {
		return relation, true
	}
	if s.parent != nil {
		return s.parent.lookupRelation(name)
	}
	return relationBinding{}, false
}

func (s *scope) localVisibleProducts() productSet {
	result := make(productSet)
	for _, relation := range s.relations {
		for _, column := range relation.Columns {
			mergeInto(result, column.Sources)
		}
	}
	return result
}

func (s *scope) relationsAddedSince(before map[string]struct{}) map[string]relationBinding {
	result := make(map[string]relationBinding)
	for name, relation := range s.relations {
		if _, existed := before[name]; !existed {
			result[name] = relation
		}
	}
	return result
}

func productRelation(product productPolicy) relationBinding {
	relation := relationBinding{
		Name:    product.grant.LogicalName,
		Columns: make(map[string]columnBinding, len(product.grant.ApprovedColumns)),
		Order:   make([]columnBinding, 0, len(product.grant.ApprovedColumns)),
	}
	for _, column := range product.grant.ApprovedColumns {
		binding := columnBinding{
			Name:       column,
			Sources:    productSet{product.grant.LogicalName: {}},
			References: map[string]struct{}{product.grant.LogicalName + "." + column: {}},
		}
		relation.Columns[column] = binding
		relation.Order = append(relation.Order, binding)
	}
	return relation
}

func relationFromOutputs(name string, columns []columnBinding) (relationBinding, error) {
	relation := relationBinding{
		Name:    name,
		Columns: make(map[string]columnBinding, len(columns)),
		Order:   make([]columnBinding, len(columns)),
	}
	for index, column := range columns {
		relation.Order[index] = column
		if column.Name == "" {
			continue
		}
		if !safeCatalogIdentifier(column.Name) {
			return relationBinding{}, reject(CodeColumnNotAllowed)
		}
		if existing, exists := relation.Columns[column.Name]; exists {
			column = mergeColumns(existing, column)
			column.Ambiguous = true
		}
		relation.Columns[column.Name] = column
	}
	return relation, nil
}

func combineJoinRelations(name string, relations map[string]relationBinding) (relationBinding, error) {
	combined := relationBinding{
		Name:    name,
		Columns: make(map[string]columnBinding),
	}
	names := make([]string, 0, len(relations))
	for relationName := range relations {
		names = append(names, relationName)
	}
	sort.Strings(names)
	for _, relationName := range names {
		relation := relations[relationName]
		for _, column := range relation.Order {
			combined.Order = append(combined.Order, column)
			if column.Name == "" {
				continue
			}
			if existing, exists := combined.Columns[column.Name]; exists {
				column = mergeColumns(existing, column)
				column.Ambiguous = true
			}
			combined.Columns[column.Name] = column
		}
	}
	return combined, nil
}

func applyAliasColumnNames(relation relationBinding, names []string) (relationBinding, error) {
	if len(names) == 0 {
		return relation, nil
	}
	if len(names) > len(relation.Order) {
		return relationBinding{}, reject(CodeColumnNotAllowed)
	}
	for _, name := range names {
		if !safeCatalogIdentifier(name) {
			return relationBinding{}, reject(CodeColumnNotAllowed)
		}
	}
	renamed := relationBinding{
		Name:    relation.Name,
		Columns: make(map[string]columnBinding, len(relation.Columns)),
		Order:   make([]columnBinding, len(relation.Order)),
	}
	copy(renamed.Order, relation.Order)
	for index, name := range names {
		renamed.Order[index].Name = name
	}
	for _, column := range renamed.Order {
		if column.Name == "" {
			continue
		}
		if existing, exists := renamed.Columns[column.Name]; exists {
			column = mergeColumns(existing, column)
			column.Ambiguous = true
		}
		renamed.Columns[column.Name] = column
	}
	return renamed, nil
}

func outputAliasMap(columns []columnBinding) map[string]columnBinding {
	result := make(map[string]columnBinding)
	for _, column := range columns {
		if column.Name == "" {
			continue
		}
		if existing, exists := result[column.Name]; exists {
			column = mergeColumns(existing, column)
			column.Ambiguous = true
		}
		result[column.Name] = column
	}
	return result
}

func outputName(value any) string {
	name, body, ok := unwrapNode(value)
	if !ok || name != "ColumnRef" {
		return ""
	}
	parts, wildcard, valid := nodeIdentifierPath(body["fields"])
	if wildcard || !valid || len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func parseAlias(value any) (string, []string, error) {
	if value == nil {
		return "", nil, nil
	}
	body, ok := value.(map[string]any)
	if !ok {
		return "", nil, reject(CodeInvalidSQL)
	}
	name, _ := body["aliasname"].(string)
	if name != "" && !safeCatalogIdentifier(name) {
		return "", nil, reject(CodeObjectNotAllowed)
	}
	columns := nodeStringList(body["colnames"])
	return name, columns, nil
}

func selectStmtBody(value any) (map[string]any, error) {
	if body, ok := value.(map[string]any); ok && looksLikeSelectBody(body) {
		return body, nil
	}
	name, body, ok := unwrapNode(value)
	if !ok {
		return nil, reject(CodeInvalidSQL)
	}
	if name != "SelectStmt" {
		if code, denied := deniedNodes[name]; denied {
			return nil, reject(code)
		}
		return nil, reject(topLevelCode(map[string]any{name: body}))
	}
	return body, nil
}

func looksLikeSelectBody(body map[string]any) bool {
	for _, key := range []string{"targetList", "fromClause", "whereClause", "larg", "rarg", "withClause", "valuesLists"} {
		if _, ok := body[key]; ok {
			return true
		}
	}
	return false
}

func unwrapNode(value any) (string, map[string]any, bool) {
	node, ok := value.(map[string]any)
	if !ok || len(node) != 1 {
		return "", nil, false
	}
	for name, raw := range node {
		body, ok := raw.(map[string]any)
		return name, body, ok
	}
	return "", nil, false
}

func isAggregateStar(body map[string]any) bool {
	star, _ := body["agg_star"].(bool)
	return star
}

func bindingFromResult(result selectResult) columnBinding {
	binding := emptyBinding()
	mergeInto(binding.Sources, result.Sources)
	for _, column := range result.Columns {
		binding = mergeColumns(binding, column)
	}
	return binding
}

func emptyBinding() columnBinding {
	return columnBinding{Sources: make(productSet), References: make(map[string]struct{})}
}

func mergeColumns(left, right columnBinding) columnBinding {
	result := columnBinding{
		Name:       left.Name,
		Sources:    copyProductSet(left.Sources),
		References: copyStringSet(left.References),
		Ambiguous:  left.Ambiguous || right.Ambiguous,
	}
	if result.Name == "" {
		result.Name = right.Name
	}
	mergeInto(result.Sources, right.Sources)
	for reference := range right.References {
		result.References[reference] = struct{}{}
	}
	return result
}

func copyProductSet(source productSet) productSet {
	result := make(productSet, len(source))
	mergeInto(result, source)
	return result
}

func mergeInto(destination, source productSet) {
	for value := range source {
		destination[value] = struct{}{}
	}
}

func copyStringSet(source map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(source))
	for value := range source {
		result[value] = struct{}{}
	}
	return result
}

func cloneRelations(source map[string]relationBinding) map[string]relationBinding {
	result := make(map[string]relationBinding, len(source))
	for name, relation := range source {
		result[name] = relation
	}
	return result
}

func relationNames(relations map[string]relationBinding) map[string]struct{} {
	result := make(map[string]struct{}, len(relations))
	for name := range relations {
		result[name] = struct{}{}
	}
	return result
}

func validateTypeCast(body map[string]any) error {
	typeName, ok := body["typeName"].(map[string]any)
	if !ok {
		return reject(CodeFeatureNotAllowed)
	}
	if bounds, ok := typeName["arrayBounds"].([]any); ok && len(bounds) != 0 {
		return reject(CodeFeatureNotAllowed)
	}
	names := nodeStringList(typeName["names"])
	if len(names) == 2 && names[0] == "pg_catalog" {
		names = names[1:]
	}
	if len(names) != 1 {
		return reject(CodeFeatureNotAllowed)
	}
	if _, safe := safeTypes[names[0]]; !safe {
		return reject(CodeFeatureNotAllowed)
	}
	return nil
}

func nodeIdentifierPath(value any) (parts []string, wildcard bool, valid bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false, false
	}
	parts = make([]string, 0, len(items))
	for _, item := range items {
		node, ok := item.(map[string]any)
		if !ok || len(node) != 1 {
			return nil, false, false
		}
		if raw, found := node["A_Star"]; found {
			if _, ok := raw.(map[string]any); !ok {
				return nil, false, false
			}
			wildcard = true
			continue
		}
		stringNode, found := node["String"]
		if !found {
			return nil, false, false
		}
		body, ok := stringNode.(map[string]any)
		if !ok {
			return nil, false, false
		}
		value, ok := body["sval"].(string)
		if !ok || value == "" {
			return nil, false, false
		}
		parts = append(parts, value)
	}
	return parts, wildcard, true
}

func nodeStringList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		node, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		stringNode, ok := node["String"].(map[string]any)
		if !ok {
			return nil
		}
		value, ok := stringNode["sval"].(string)
		if !ok || value == "" {
			return nil
		}
		result = append(result, value)
	}
	return result
}

func systemRelation(name string) bool {
	return name == "information_schema" || name == "pg_catalog" || strings.HasPrefix(name, "pg_")
}

func dangerousFunction(name string) bool {
	if _, found := dangerousFunctions[name]; found {
		return true
	}
	for _, prefix := range dangerousFunctionPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

var writeNodes = map[string]struct{}{
	"AlterTableStmt": {}, "CallStmt": {}, "CopyStmt": {}, "CreateStmt": {}, "DeleteStmt": {},
	"DoStmt": {}, "DropStmt": {}, "InsertStmt": {}, "MergeStmt": {}, "PlassignStmt": {},
	"RefreshMatViewStmt": {}, "TruncateStmt": {}, "UpdateStmt": {}, "VariableSetStmt": {},
}

var deniedNodes = map[string]Code{
	"A_Star": CodeWildcard, "Param": CodeParameter, "ParamRef": CodeParameter,
	"IntoClause": CodeSelectInto, "LockingClause": CodeLocking,
	"RangeFunction": CodeFeatureNotAllowed, "RangeTableFunc": CodeFeatureNotAllowed,
	"RangeTableSample": CodeFeatureNotAllowed, "SQLValueFunction": CodeParameter,
	"TableFunc": CodeFeatureNotAllowed, "JsonTable": CodeFeatureNotAllowed,
	"AlterTableStmt": CodeWriteForbidden, "CallStmt": CodeWriteForbidden,
	"CopyStmt": CodeWriteForbidden, "CreateStmt": CodeWriteForbidden,
	"DeleteStmt": CodeWriteForbidden, "DoStmt": CodeWriteForbidden,
	"DropStmt": CodeWriteForbidden, "InsertStmt": CodeWriteForbidden,
	"MergeStmt": CodeWriteForbidden, "PlassignStmt": CodeWriteForbidden,
	"RefreshMatViewStmt": CodeWriteForbidden, "TruncateStmt": CodeWriteForbidden,
	"UpdateStmt": CodeWriteForbidden, "VariableSetStmt": CodeWriteForbidden,
}

var allowedNodes = map[string]struct{}{
	"A_ArrayExpr": {}, "A_Const": {}, "A_Expr": {}, "A_Indices": {}, "A_Indirection": {},
	"BitString": {}, "Boolean": {}, "BooleanTest": {}, "BoolExpr": {},
	"CaseExpr": {}, "CaseWhen": {}, "CoalesceExpr": {}, "ColumnRef": {},
	"CommonTableExpr": {}, "Float": {}, "FuncCall": {}, "GroupingFunc": {},
	"GroupingSet": {}, "Integer": {}, "JoinExpr": {}, "List": {}, "MinMaxExpr": {},
	"NamedArgExpr": {}, "NullTest": {}, "RangeSubselect": {}, "RangeVar": {},
	"ResTarget": {}, "RowExpr": {}, "SelectStmt": {}, "SortBy": {}, "String": {},
	"SubLink": {}, "TypeCast": {}, "WindowDef": {},
}

var dangerousFunctions = map[string]struct{}{
	"current_setting": {}, "set_config": {}, "nextval": {}, "setval": {}, "currval": {},
	"pg_advisory_lock": {}, "pg_advisory_lock_shared": {}, "pg_advisory_unlock": {},
	"pg_advisory_unlock_all": {}, "pg_advisory_unlock_shared": {}, "pg_cancel_backend": {},
	"pg_notify": {}, "pg_reload_conf": {}, "pg_rotate_logfile": {}, "pg_sleep": {},
	"pg_sleep_for": {}, "pg_sleep_until": {}, "pg_terminate_backend": {},
	"lo_export": {}, "lo_import": {}, "dblink": {}, "dblink_exec": {},
	// ts_stat executes the SQL text supplied by its caller. Letting a catalog
	// enable it would create a second SQL parser/execution path that bypasses
	// logical-product, column, and mandatory-scope validation.
	"ts_stat": {},
}

var dangerousFunctionPrefixes = []string{
	// PostgreSQL's XML mapping helpers either execute caller-supplied SQL or
	// export a table, schema, database, or cursor. They must remain denied even
	// when a trusted catalog accidentally includes them in an allowlist.
	"cursor_to_xml", "database_to_xml", "query_to_xml", "schema_to_xml", "table_to_xml",
	"dblink_", "lo_", "pg_backup_", "pg_create_restore_point", "pg_file_", "pg_log_",
	"pg_ls_", "pg_promote", "pg_read_", "pg_replication_", "pg_stat_file", "pg_switch_wal",
	"pg_wal_replay_",
}

var safeTypes = map[string]struct{}{
	"bool": {}, "boolean": {}, "int2": {}, "smallint": {}, "int4": {}, "int": {},
	"integer": {}, "int8": {}, "bigint": {}, "float4": {}, "real": {}, "float8": {},
	"numeric": {}, "decimal": {}, "text": {}, "varchar": {}, "bpchar": {}, "date": {},
	"time": {}, "timetz": {}, "timestamp": {}, "timestamptz": {}, "interval": {}, "uuid": {},
	"json": {}, "jsonb": {},
}
