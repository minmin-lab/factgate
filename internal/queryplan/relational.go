package queryplan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

// MaxJoinSources is an operational complexity guard, not a restriction on
// graph shape. It bounds generated SQL width, provenance rows, and downstream
// PostgreSQL planning work while admitting practical multi-hop join graphs.
const MaxJoinSources = 16

// RelationalCompilation is the paired SQL and trusted metadata consumed by
// the online positive-output dependency path. ProvenanceSQL returns positive
// source members, never a representative chosen after UNION DISTINCT.
type RelationalCompilation struct {
	VisibleSQL       string
	ProvenanceSQL    string
	VisibleFields    []string
	InternalFields   []string
	ProvenanceFields []string
	Products         []string
	Kind             string
	ExpandedEvidence bool
	Sources          []RelationalSource
	JoinPredicates   []JoinPredicate
	UnionColumns     []string
	OutputAliases    map[string]string
	OrdinalProgram   OrdinalProgram
}

type RelationalSource struct {
	Product        string
	Role           string
	Filters        []Filter
	EvidenceFields []string
	EvidenceAlias  map[string]string
	Branch         int
}

// CompileRelational lowers the public closed grammar to one visible statement
// and a same-snapshot provenance companion. Product contains Catalog metadata
// plus the task-approved columns, so every identifier and type is checked
// before SQL exists.
func CompileRelational(plan QueryPlan, products map[string]Product) (RelationalCompilation, error) {
	if plan.From == nil || plan.Product != "" {
		return RelationalCompilation{}, errors.New("relational QueryPlan requires from and forbids legacy product")
	}
	canonical, err := CanonicalizeRelational(plan, products)
	if err != nil {
		return RelationalCompilation{}, err
	}
	plan = canonical
	if plan.Limit != 0 || plan.Offset != 0 {
		return RelationalCompilation{}, errors.New("LIMIT and OFFSET are outside the online Join/Union fragment")
	}
	if len(plan.Columns)+len(plan.Aggregates) == 0 {
		return RelationalCompilation{}, errors.New("empty select list")
	}
	if countFromMembers(*plan.From) != 1 {
		return RelationalCompilation{}, errors.New("from must contain exactly one operator")
	}

	var result RelationalCompilation
	result.OutputAliases = make(map[string]string)
	var fields map[string]relationalField
	var fromSQL, provenanceFrom string
	switch {
	case plan.From.Scan != nil:
		compiled, schema, err := compileExplicitScan(*plan.From.Scan, products)
		if err != nil {
			return result, err
		}
		compiled.source.EvidenceFields = evidenceFields(*plan.From.Scan, products[plan.From.Scan.Product], schema, plan, nil, nil)
		compiled.source.EvidenceAlias = evidenceAliases(plan.From.Scan.Role, compiled.source.EvidenceFields)
		result.Kind, result.Sources = "scan", []RelationalSource{compiled.source}
		fields, fromSQL, provenanceFrom = schema, compiled.sql, compiled.sql
	case plan.From.JoinMany != nil:
		joinResult, schema, visibleFrom, provenanceSQL, err := compileJoinMany(*plan.From.JoinMany, plan, products)
		if err != nil {
			return result, err
		}
		result.Kind, result.Sources, result.JoinPredicates = "join", joinResult.sources, append([]JoinPredicate(nil), plan.From.JoinMany.On...)
		fields, fromSQL, provenanceFrom = schema, visibleFrom, provenanceSQL
	case plan.From.UnionDistinct != nil:
		unionResult, schema, visibleFrom, provenanceSQL, err := compileUnion(*plan.From.UnionDistinct, plan.Filters, products)
		if err != nil {
			return result, err
		}
		result.Kind, result.Sources, result.UnionColumns = "union_distinct", unionResult.sources, append([]string(nil), plan.From.UnionDistinct.Columns...)
		result.ExpandedEvidence = true
		fields, fromSQL, provenanceFrom = schema, visibleFrom, provenanceSQL
	}
	if (len(plan.OrderBy) != 0 || hasAggregateResultEncoding(plan.Aggregates)) && result.Kind != "join" {
		return RelationalCompilation{}, errors.New("group-key delivery order and aggregate result encoding are limited to joined relational plans")
	}
	if hasAggregateResultEncoding(plan.Aggregates) && len(plan.GroupBy) == 0 {
		return RelationalCompilation{}, errors.New("aggregate result encoding requires a grouped joined relational plan")
	}
	if hasAggregateResultEncoding(plan.Aggregates) && len(plan.OrderBy) == 0 {
		return RelationalCompilation{}, errors.New("aggregate result encoding requires the complete grouped result order")
	}

	selectSQL, internal, visible, aliases, err := compileRelationalSelect(plan, fields, fromSQL)
	if err != nil {
		return RelationalCompilation{}, err
	}
	orderSQL, err := compileRelationalVisibleOrder(plan, fields, aliases)
	if err != nil {
		return RelationalCompilation{}, err
	}
	selectSQL += orderSQL
	result.VisibleSQL = selectSQL
	result.OutputAliases = aliases
	result.InternalFields = internal
	result.VisibleFields = visible
	if result.Kind == "scan" {
		// Explicit scan has no set operator membership expansion. Its companion
		// is built below from all evidence fields just like the join companion.
		provenanceFrom = fromSQL
	}
	if result.Kind != "union_distinct" {
		provenanceSelect := make([]string, 0)
		seenAliases := make(map[string]struct{})
		for _, source := range result.Sources {
			for _, field := range source.EvidenceFields {
				alias := source.EvidenceAlias[field]
				if len(alias) > 63 {
					return RelationalCompilation{}, errors.New("provenance alias exceeds the PostgreSQL identifier limit")
				}
				if _, duplicate := seenAliases[alias]; duplicate {
					return RelationalCompilation{}, errors.New("provenance aliases collide")
				}
				seenAliases[alias] = struct{}{}
				provenanceSelect = append(provenanceSelect, qualified(source.Role, field)+" AS "+quoteIdentifier(alias))
				result.ProvenanceFields = append(result.ProvenanceFields, alias)
			}
		}
		result.ProvenanceSQL = "SELECT " + strings.Join(provenanceSelect, ", ") + " FROM " + provenanceFrom
		if len(plan.Filters) > 0 {
			where, whereErr := compileQualifiedFilters(plan.Filters, fields)
			if whereErr != nil {
				return RelationalCompilation{}, whereErr
			}
			result.ProvenanceSQL += " WHERE " + where
		}
	} else {
		result.ProvenanceSQL = provenanceFrom
		result.ProvenanceFields = append(result.ProvenanceFields, "tg_branch")
		for _, field := range result.Sources[0].EvidenceFields {
			alias := result.Sources[0].EvidenceAlias[field]
			if len(alias) > 63 {
				return RelationalCompilation{}, errors.New("provenance alias exceeds the PostgreSQL identifier limit")
			}
			result.ProvenanceFields = append(result.ProvenanceFields, alias)
		}
	}
	grouped := len(plan.GroupBy) > 0 || len(plan.Aggregates) > 0
	var provenanceOrder []OrdinalOrderSpec
	if !grouped && (result.Kind == "scan" || result.Kind == "join") {
		orders := make([]string, 0)
		for _, source := range result.Sources {
			product := products[source.Product]
			for _, key := range product.StableEntityKey {
				orders = append(orders, qualified(source.Role, key)+" ASC")
				provenanceOrder = append(provenanceOrder, OrdinalOrderSpec{Kind: "entity", FieldID: source.Role + "." + key,
					SourceAlias: source.Role, ProvenanceAlias: source.EvidenceAlias[key], Direction: "ASC"})
			}
		}
		if len(orders) == 0 {
			return RelationalCompilation{}, errors.New("relational row delivery requires Catalog stable entity keys")
		}
		orderSQL := " ORDER BY " + strings.Join(orders, ", ")
		result.VisibleSQL += orderSQL
		result.ProvenanceSQL += orderSQL
	}
	if grouped {
		orderSQL, order, orderErr := relationalGroupedProvenanceOrder(plan, result, products)
		if orderErr != nil {
			return RelationalCompilation{}, orderErr
		}
		result.ProvenanceSQL += orderSQL
		provenanceOrder = order
	} else if result.Kind == "union_distinct" {
		// Alternative proofs for one DISTINCT tuple must be contiguous so the
		// online engine can apply exact max semantics with O(1) tuple state.
		// Visible SQL remains unordered; only the provenance companion receives
		// this canonical full-tuple/entity/branch order.
		orderSQL, order, orderErr := relationalGroupedProvenanceOrder(plan, result, products)
		if orderErr != nil {
			return RelationalCompilation{}, orderErr
		}
		result.ProvenanceSQL += orderSQL
		provenanceOrder = order
	}
	result.Products = relationalProducts(result.Sources)
	result.OrdinalProgram, err = buildRelationalOrdinalProgram(plan, products, result, provenanceOrder)
	if err != nil {
		return RelationalCompilation{}, err
	}
	return result, nil
}

type relationalField struct {
	ID                   string
	Role                 string
	Column               string
	SQLType              string
	Collation            string
	CollationVersion     string
	Product              string
	AggregatePermissions map[string]struct{}
}

type compiledScan struct {
	sql    string
	source RelationalSource
}

type compiledRelation struct{ sources []RelationalSource }

// CanonicalizeRelational erases the legacy binary-join shape and orders the
// flat join graph by Catalog semantic source identity. It deliberately leaves
// projection order intact because that order is part of the delivered result,
// while predicate conjunction order is not.
func CanonicalizeRelational(plan QueryPlan, products map[string]Product) (QueryPlan, error) {
	if plan.From == nil || plan.Product != "" || countFromMembers(*plan.From) != 1 {
		return QueryPlan{}, errors.New("relational QueryPlan requires exactly one from operator")
	}
	result := plan
	result.Columns = append([]string(nil), plan.Columns...)
	result.Aggregates = append([]Aggregate(nil), plan.Aggregates...)
	result.GroupBy = append([]string(nil), plan.GroupBy...)
	result.OrderBy = append([]Order(nil), plan.OrderBy...)
	var err error
	result.Filters, err = canonicalizeFilters(plan.Filters)
	if err != nil {
		return QueryPlan{}, err
	}
	from := &From{}
	switch {
	case plan.From.Scan != nil:
		scan, scanErr := canonicalizeScan(*plan.From.Scan)
		if scanErr != nil {
			return QueryPlan{}, scanErr
		}
		from.Scan = &scan
	case plan.From.Join != nil:
		from.JoinMany = &JoinMany{Sources: []Scan{plan.From.Join.Left, plan.From.Join.Right}, On: append([]JoinPredicate(nil), plan.From.Join.On...)}
	case plan.From.JoinMany != nil:
		join := *plan.From.JoinMany
		join.Sources = append([]Scan(nil), join.Sources...)
		join.On = append([]JoinPredicate(nil), join.On...)
		from.JoinMany = &join
	case plan.From.UnionDistinct != nil:
		union := *plan.From.UnionDistinct
		union.Columns = append([]string(nil), union.Columns...)
		var scanErr error
		union.Left, scanErr = canonicalizeScan(union.Left)
		if scanErr != nil {
			return QueryPlan{}, scanErr
		}
		union.Right, scanErr = canonicalizeScan(union.Right)
		if scanErr != nil {
			return QueryPlan{}, scanErr
		}
		from.UnionDistinct = &union
	}
	if from.JoinMany != nil {
		for index := range from.JoinMany.Sources {
			from.JoinMany.Sources[index], err = canonicalizeScan(from.JoinMany.Sources[index])
			if err != nil {
				return QueryPlan{}, err
			}
			if _, present := products[from.JoinMany.Sources[index].Product]; !present {
				return QueryPlan{}, fmt.Errorf("join source product %q is not approved", from.JoinMany.Sources[index].Product)
			}
		}
		sort.Slice(from.JoinMany.Sources, func(i, j int) bool {
			return relationalScanSemanticKey(from.JoinMany.Sources[i], products) < relationalScanSemanticKey(from.JoinMany.Sources[j], products)
		})
		for index, predicate := range from.JoinMany.On {
			if _, _, ok := splitFieldID(predicate.Left); !ok {
				return QueryPlan{}, fmt.Errorf("join predicate field %q is invalid", predicate.Left)
			}
			if _, _, ok := splitFieldID(predicate.Right); !ok {
				return QueryPlan{}, fmt.Errorf("join predicate field %q is invalid", predicate.Right)
			}
			if predicate.Right < predicate.Left {
				predicate.Left, predicate.Right = predicate.Right, predicate.Left
			}
			from.JoinMany.On[index] = predicate
		}
		sort.Slice(from.JoinMany.On, func(i, j int) bool {
			left := from.JoinMany.On[i].Left + "\x00" + from.JoinMany.On[i].Right
			right := from.JoinMany.On[j].Left + "\x00" + from.JoinMany.On[j].Right
			return left < right
		})
	}
	result.From = from
	return result, nil
}

func canonicalizeScan(scan Scan) (Scan, error) {
	filters, err := canonicalizeFilters(scan.Filters)
	if err != nil {
		return Scan{}, err
	}
	scan.Filters = filters
	return scan, nil
}

func canonicalizeFilters(filters []Filter) ([]Filter, error) {
	result := append([]Filter(nil), filters...)
	keys := make([]string, len(result))
	for index := range result {
		result[index].Op = strings.ToUpper(strings.TrimSpace(result[index].Op))
		if result[index].Op == "!=" {
			result[index].Op = "<>"
		}
		encoded, err := canonicalJSON(result[index].Value)
		if err != nil {
			return nil, fmt.Errorf("canonicalize filter %q: %w", result[index].Column, err)
		}
		keys[index] = result[index].Column + "\x00" + result[index].Op + "\x00" + string(encoded)
	}
	indices := make([]int, len(result))
	for index := range indices {
		indices[index] = index
	}
	sort.SliceStable(indices, func(i, j int) bool { return keys[indices[i]] < keys[indices[j]] })
	canonical := make([]Filter, len(result))
	for index, source := range indices {
		canonical[index] = result[source]
	}
	return canonical, nil
}

func relationalScanSemanticKey(scan Scan, products map[string]Product) string {
	product := products[scan.Product]
	return product.SourceNamespace + "\x00" + product.Snapshot + "\x00" + product.StableRole + "\x00" + product.Name
}

func compileExplicitScan(scan Scan, products map[string]Product) (compiledScan, map[string]relationalField, error) {
	product, present := products[scan.Product]
	if !present || product.Name != scan.Product {
		return compiledScan{}, nil, fmt.Errorf("scan product %q is not approved", scan.Product)
	}
	if !safeIdentifier(scan.Product) || !safeRole(scan.Role) {
		return compiledScan{}, nil, errors.New("scan product or role is invalid")
	}
	if product.SourceNamespace == "" || product.Snapshot == "" || len(product.StableEntityKey) == 0 {
		return compiledScan{}, nil, errors.New("scan product lacks V2 namespace, snapshot, or stable entity key")
	}
	if err := validateProductSQLProfileV2(product); err != nil {
		return compiledScan{}, nil, err
	}
	if scan.Role != product.StableRole {
		return compiledScan{}, nil, errors.New("scan role must equal the Catalog stable relation role")
	}
	schema := make(map[string]relationalField, len(product.Columns))
	for column := range product.Columns {
		id := scan.Role + "." + column
		schema[id] = fieldFromProduct(id, scan.Role, column, product)
	}
	if _, err := compileScanFilters(scan, product); err != nil {
		return compiledScan{}, nil, err
	}
	sql := quoteIdentifier(scan.Product) + " AS " + quoteIdentifier(scan.Role)
	if len(scan.Filters) > 0 {
		where, _ := compileLeafFilters(scan, product)
		// A subquery keeps a leaf predicate local when this scan later becomes a
		// grammar building block; explicit top-level scan uses the same form.
		selected := make(map[string]struct{}, len(product.Columns)+len(product.StableEntityKey)+len(product.RequiredEvidence))
		for column := range product.Columns {
			selected[column] = struct{}{}
		}
		for _, column := range product.StableEntityKey {
			selected[column] = struct{}{}
		}
		for _, column := range product.RequiredEvidence {
			selected[column] = struct{}{}
		}
		selects := make([]string, 0, len(selected))
		for _, column := range sortedColumns(selected) {
			selects = append(selects, quoteIdentifier(column))
		}
		sql = "(SELECT " + strings.Join(selects, ", ") + " FROM " + quoteIdentifier(scan.Product) + " WHERE " + where + ") AS " + quoteIdentifier(scan.Role)
	}
	return compiledScan{sql: sql, source: RelationalSource{Product: scan.Product, Role: scan.Role, Filters: cloneFilters(scan.Filters)}}, schema, nil
}

func compileJoinMany(join JoinMany, plan QueryPlan, products map[string]Product) (compiledRelation, map[string]relationalField, string, string, error) {
	if len(join.Sources) < 2 || len(join.Sources) > MaxJoinSources {
		return compiledRelation{}, nil, "", "", fmt.Errorf("join_many requires between 2 and %d sources", MaxJoinSources)
	}
	if len(join.On) == 0 {
		return compiledRelation{}, nil, "", "", errors.New("join_many requires at least one equality")
	}

	type joinSource struct {
		scan     Scan
		compiled compiledScan
		schema   map[string]relationalField
	}
	sources := make([]joinSource, 0, len(join.Sources))
	byRole := make(map[string]int, len(join.Sources))
	seenProducts := make(map[string]struct{}, len(join.Sources))
	schema := make(map[string]relationalField)
	for _, scan := range join.Sources {
		if _, duplicate := seenProducts[scan.Product]; duplicate {
			return compiledRelation{}, nil, "", "", errors.New("join_many does not support repeated products or self-joins")
		}
		seenProducts[scan.Product] = struct{}{}
		if _, duplicate := byRole[scan.Role]; duplicate {
			return compiledRelation{}, nil, "", "", errors.New("join_many source roles must be unique")
		}
		compiled, sourceSchema, err := compileExplicitScan(scan, products)
		if err != nil {
			return compiledRelation{}, nil, "", "", err
		}
		byRole[scan.Role] = len(sources)
		for id, field := range sourceSchema {
			if _, duplicate := schema[id]; duplicate {
				return compiledRelation{}, nil, "", "", fmt.Errorf("join_many field %q is ambiguous", id)
			}
			schema[id] = field
		}
		sources = append(sources, joinSource{scan: scan, compiled: compiled, schema: sourceSchema})
	}

	type joinEdge struct {
		predicate JoinPredicate
		leftRole  string
		rightRole string
		sql       string
	}
	edges := make([]joinEdge, 0, len(join.On))
	seenPredicates := make(map[string]struct{}, len(join.On))
	parents := make([]int, len(sources))
	for index := range parents {
		parents[index] = index
	}
	var find func(int) int
	find = func(value int) int {
		if parents[value] != value {
			parents[value] = find(parents[value])
		}
		return parents[value]
	}
	union := func(left, right int) {
		left, right = find(left), find(right)
		if left != right {
			parents[right] = left
		}
	}
	for _, predicate := range join.On {
		leftRole, _, leftValid := splitFieldID(predicate.Left)
		rightRole, _, rightValid := splitFieldID(predicate.Right)
		leftIndex, leftRolePresent := byRole[leftRole]
		rightIndex, rightRolePresent := byRole[rightRole]
		leftField, leftFieldPresent := schema[predicate.Left]
		rightField, rightFieldPresent := schema[predicate.Right]
		if !leftValid || !rightValid || !leftRolePresent || !rightRolePresent || !leftFieldPresent || !rightFieldPresent || leftRole == rightRole {
			return compiledRelation{}, nil, "", "", errors.New("join predicate must reference fields from two distinct sources")
		}
		key := predicate.Left + "\x00" + predicate.Right
		if _, duplicate := seenPredicates[key]; duplicate {
			return compiledRelation{}, nil, "", "", errors.New("duplicate join predicate")
		}
		seenPredicates[key] = struct{}{}
		if leftField.SQLType != rightField.SQLType || leftField.Collation != rightField.Collation ||
			leftField.CollationVersion != rightField.CollationVersion {
			return compiledRelation{}, nil, "", "", errors.New("join keys require identical types and deterministic collation profiles")
		}
		edges = append(edges, joinEdge{predicate: predicate, leftRole: leftRole, rightRole: rightRole,
			sql: fieldSQL(leftField) + " = " + fieldSQL(rightField)})
		union(leftIndex, rightIndex)
	}
	root := find(0)
	for index := 1; index < len(sources); index++ {
		if find(index) != root {
			return compiledRelation{}, nil, "", "", errors.New("join_many graph must be connected")
		}
	}
	if _, err := compileQualifiedFilters(plan.Filters, schema); err != nil {
		return compiledRelation{}, nil, "", "", err
	}

	joined := map[string]struct{}{sources[0].scan.Role: {}}
	joinOrder := []int{0}
	from := sources[0].compiled.sql
	for len(joinOrder) < len(sources) {
		next := -1
		for index, source := range sources {
			if _, present := joined[source.scan.Role]; present {
				continue
			}
			connected := false
			for _, edge := range edges {
				if edge.leftRole == source.scan.Role {
					_, connected = joined[edge.rightRole]
				} else if edge.rightRole == source.scan.Role {
					_, connected = joined[edge.leftRole]
				}
				if connected {
					break
				}
			}
			if connected {
				next = index
				break
			}
		}
		if next < 0 {
			return compiledRelation{}, nil, "", "", errors.New("join_many graph cannot be compiled in canonical order")
		}
		role := sources[next].scan.Role
		conditions := make([]string, 0)
		for _, edge := range edges {
			other := ""
			if edge.leftRole == role {
				other = edge.rightRole
			} else if edge.rightRole == role {
				other = edge.leftRole
			}
			if _, present := joined[other]; other != "" && present {
				conditions = append(conditions, edge.sql)
			}
		}
		sort.Strings(conditions)
		if len(conditions) == 0 {
			return compiledRelation{}, nil, "", "", errors.New("join_many source has no edge to the compiled component")
		}
		from += " INNER JOIN " + sources[next].compiled.sql + " ON " + strings.Join(conditions, " AND ")
		joined[role] = struct{}{}
		joinOrder = append(joinOrder, next)
	}

	resultSources := make([]RelationalSource, 0, len(sources))
	for _, index := range joinOrder {
		source := sources[index]
		source.compiled.source.EvidenceFields = evidenceFields(source.scan, products[source.scan.Product], schema, plan, join.On, nil)
		source.compiled.source.EvidenceAlias = evidenceAliases(source.scan.Role, source.compiled.source.EvidenceFields)
		resultSources = append(resultSources, source.compiled.source)
	}
	return compiledRelation{sources: resultSources}, schema, from, from, nil
}

func compileUnion(union UnionDistinct, outerFilters []Filter, products map[string]Product) (compiledRelation, map[string]relationalField, string, string, error) {
	if !safeRole(union.Role) || !safeRole(union.Left.Role) || !safeRole(union.Right.Role) ||
		union.Left.Role == union.Right.Role || union.Left.Product != union.Right.Product {
		return compiledRelation{}, nil, "", "", errors.New("UNION DISTINCT requires one product and distinct valid branch and output roles")
	}
	product, present := products[union.Left.Product]
	if !present {
		return compiledRelation{}, nil, "", "", errors.New("UNION DISTINCT product is not approved")
	}
	if union.Role != product.StableRole {
		return compiledRelation{}, nil, "", "", errors.New("UNION DISTINCT output role must equal the Catalog stable relation role")
	}
	if len(union.Columns) == 0 {
		return compiledRelation{}, nil, "", "", errors.New("UNION DISTINCT requires its complete deduplication schema")
	}
	columns := uniqueOrdered(union.Columns)
	if len(columns) != len(union.Columns) {
		return compiledRelation{}, nil, "", "", errors.New("UNION DISTINCT repeats a deduplication column")
	}
	schema := make(map[string]relationalField, len(columns))
	for _, column := range columns {
		if err := allowedColumn(column, product); err != nil {
			return compiledRelation{}, nil, "", "", err
		}
		id := union.Role + "." + column
		schema[id] = fieldFromProduct(id, union.Role, column, product)
	}
	if _, err := compileQualifiedFilters(outerFilters, schema); err != nil {
		return compiledRelation{}, nil, "", "", err
	}
	for _, scan := range []Scan{union.Left, union.Right} {
		if _, err := compileScanFilters(scan, product); err != nil {
			return compiledRelation{}, nil, "", "", err
		}
	}
	evidenceSet := make(map[string]struct{})
	for _, column := range columns {
		evidenceSet[column] = struct{}{}
	}
	for _, column := range product.StableEntityKey {
		evidenceSet[column] = struct{}{}
	}
	for _, column := range product.RequiredEvidence {
		if err := ordinalProductField(column, product); err != nil {
			return compiledRelation{}, nil, "", "", err
		}
		evidenceSet[column] = struct{}{}
	}
	for _, scan := range []Scan{union.Left, union.Right} {
		for _, filter := range scan.Filters {
			evidenceSet[filter.Column] = struct{}{}
		}
	}
	evidence := sortedColumns(evidenceSet)
	branchesVisible := make([]string, 0, 2)
	branchesProvenance := make([]string, 0, 2)
	sources := []RelationalSource{
		{Product: union.Left.Product, Role: union.Left.Role, Filters: cloneFilters(union.Left.Filters), EvidenceFields: evidence, EvidenceAlias: evidenceAliases(union.Role, evidence), Branch: 0},
		{Product: union.Right.Product, Role: union.Right.Role, Filters: cloneFilters(union.Right.Filters), EvidenceFields: evidence, EvidenceAlias: evidenceAliases(union.Role, evidence), Branch: 1},
	}
	for branchIndex, scan := range []Scan{union.Left, union.Right} {
		visibleSelect := make([]string, 0, len(columns))
		for _, column := range columns {
			visibleSelect = append(visibleSelect, qualified(scan.Role, column)+" AS "+quoteIdentifier(column))
		}
		whereParts := make([]string, 0, 2)
		if len(scan.Filters) > 0 {
			leaf, _ := compileLeafFilters(scan, product)
			whereParts = append(whereParts, leaf)
		}
		// Applying a literal predicate before DISTINCT is equivalent to applying
		// it after DISTINCT and keeps every delivered tuple member in provenance.
		if len(outerFilters) > 0 {
			mapped := make([]Filter, 0, len(outerFilters))
			for _, filter := range outerFilters {
				_, column, _ := splitFieldID(filter.Column)
				filter.Column = column
				mapped = append(mapped, filter)
			}
			outer, _ := compileLeafFilterList(mapped, scan.Role, product)
			whereParts = append(whereParts, outer)
		}
		where := ""
		if len(whereParts) > 0 {
			where = " WHERE " + strings.Join(whereParts, " AND ")
		}
		branchesVisible = append(branchesVisible, "SELECT "+strings.Join(visibleSelect, ", ")+" FROM "+quoteIdentifier(scan.Product)+" AS "+quoteIdentifier(scan.Role)+where)
		provSelect := []string{strconv.Itoa(branchIndex) + " AS " + quoteIdentifier("tg_branch")}
		for _, column := range evidence {
			provSelect = append(provSelect, qualified(scan.Role, column)+" AS "+quoteIdentifier(evidenceAliases(union.Role, evidence)[column]))
		}
		branchesProvenance = append(branchesProvenance, "SELECT "+strings.Join(provSelect, ", ")+" FROM "+quoteIdentifier(scan.Product)+" AS "+quoteIdentifier(scan.Role)+where)
	}
	from := "(" + strings.Join(branchesVisible, " UNION ") + ") AS " + quoteIdentifier(union.Role)
	provenance := strings.Join(branchesProvenance, " UNION ALL ")
	return compiledRelation{sources: sources}, schema, from, provenance, nil
}

func compileRelationalSelect(plan QueryPlan, fields map[string]relationalField, fromSQL string) (string, []string, []string, map[string]string, error) {
	selectNames := make(map[string]struct{})
	visible := make([]string, 0, len(plan.Columns)+len(plan.Aggregates))
	internal := make([]string, 0, len(fields)+len(plan.Aggregates))
	selects := make([]string, 0, len(fields)+len(plan.Aggregates))
	requested := make(map[string]struct{})
	aliases := make(map[string]string, len(fields)+len(plan.Aggregates))
	for _, id := range plan.Columns {
		field, present := fields[id]
		if !present {
			return "", nil, nil, nil, fmt.Errorf("column %q is not in the relation schema", id)
		}
		if _, duplicate := requested[id]; duplicate {
			return "", nil, nil, nil, fmt.Errorf("duplicate select name %q", id)
		}
		requested[id] = struct{}{}
		selectNames[id] = struct{}{}
		visible = append(visible, id)
		_ = field
	}
	grouped := len(plan.GroupBy) > 0 || len(plan.Aggregates) > 0
	selectedFields := append([]string(nil), plan.Columns...)
	if !grouped && plan.From != nil && plan.From.UnionDistinct != nil {
		// Hidden relation fields carry the complete UNION DISTINCT tuple;
		// visibleResult strips them after accounting.
		selectedFields = sortedRelationalFieldIDs(fields)
	}
	if grouped {
		for _, group := range plan.GroupBy {
			if _, present := fields[group]; !present {
				return "", nil, nil, nil, fmt.Errorf("group field %q is absent", group)
			}
			if _, present := requested[group]; !present {
				selectedFields = append(selectedFields, group)
			}
		}
	}
	selectedFields = uniqueOrdered(selectedFields)
	for _, id := range selectedFields {
		field := fields[id]
		alias := relationalOutputAlias(id)
		aliases[id] = alias
		selects = append(selects, fieldSQL(field)+" AS "+quoteIdentifier(alias))
		internal = append(internal, id)
	}
	aggregateSQL := make(map[string]string, len(plan.Aggregates))
	for _, aggregate := range plan.Aggregates {
		fn := strings.ToLower(strings.TrimSpace(aggregate.Function))
		if !safeIdentifier(aggregate.Alias) || strings.HasPrefix(aggregate.Alias, "tg_") {
			return "", nil, nil, nil, errors.New("aggregate alias is invalid")
		}
		if _, duplicate := selectNames[aggregate.Alias]; duplicate {
			return "", nil, nil, nil, fmt.Errorf("duplicate select name %q", aggregate.Alias)
		}
		argument := "*"
		inputType := ""
		if aggregate.Column != "*" {
			field, present := fields[aggregate.Column]
			if !present {
				return "", nil, nil, nil, fmt.Errorf("aggregate field %q is absent", aggregate.Column)
			}
			if _, allowed := field.AggregatePermissions[fn]; !allowed {
				return "", nil, nil, nil, fmt.Errorf("aggregate %q is not approved", fn)
			}
			argument = fieldSQL(field)
			inputType = field.SQLType
		} else if fn != "count" {
			return "", nil, nil, nil, fmt.Errorf("aggregate %q does not accept *", fn)
		}
		if aggregate.Column == "*" {
			for _, field := range fields {
				if _, ok := field.AggregatePermissions[fn]; !ok {
					return "", nil, nil, nil, fmt.Errorf("aggregate %q is not approved by every input", fn)
				}
			}
		}
		naturalType := AggregateOutputType(fn, inputType)
		if naturalType == "" {
			return "", nil, nil, nil, fmt.Errorf("aggregate %q has no canonical output type", fn)
		}
		resultEncoding, encodingErr := canonicalAggregateResultEncoding(fn, naturalType, aggregate.ResultEncoding)
		if encodingErr != nil {
			return "", nil, nil, nil, encodingErr
		}
		argument, argumentErr := aggregateSQLArgument(aggregate, argument)
		if argumentErr != nil {
			return "", nil, nil, nil, argumentErr
		}
		expression := fn + "(" + argument + ")"
		aggregateSQL[aggregate.Alias] = expression
		if resultEncoding != "" {
			expression = "CAST(" + expression + " AS text)"
		}
		selects = append(selects, expression+" AS "+quoteIdentifier(aggregate.Alias))
		aliases[aggregate.Alias] = aggregate.Alias
		internal = append(internal, aggregate.Alias)
		visible = append(visible, aggregate.Alias)
		selectNames[aggregate.Alias] = struct{}{}
	}
	if len(selects) == 0 {
		return "", nil, nil, nil, errors.New("empty select list")
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(strings.Join(selects, ", "))
	b.WriteString(" FROM ")
	b.WriteString(fromSQL)
	if len(plan.Filters) > 0 {
		where, err := compileQualifiedFilters(plan.Filters, fields)
		if err != nil {
			return "", nil, nil, nil, err
		}
		b.WriteString(" WHERE ")
		b.WriteString(where)
	}
	if grouped {
		groups := uniqueOrdered(plan.GroupBy)
		if len(groups) != len(plan.GroupBy) {
			return "", nil, nil, nil, errors.New("duplicate group field")
		}
		selectedSet := make(map[string]struct{}, len(plan.GroupBy))
		for _, group := range groups {
			selectedSet[group] = struct{}{}
		}
		for _, column := range plan.Columns {
			if _, ok := selectedSet[column]; !ok {
				return "", nil, nil, nil, fmt.Errorf("selected column %q is not grouped", column)
			}
		}
		if len(groups) > 0 {
			parts := make([]string, 0, len(groups))
			for _, group := range groups {
				parts = append(parts, fieldSQL(fields[group]))
			}
			b.WriteString(" GROUP BY ")
			b.WriteString(strings.Join(parts, ", "))
		} else if len(plan.Columns) > 0 {
			return "", nil, nil, nil, errors.New("selected columns require group_by")
		}
		if len(plan.Having) > 0 {
			havings := make([]string, 0, len(plan.Having))
			for _, filter := range plan.Having {
				expression, present := aggregateSQL[filter.Column]
				if !present {
					return "", nil, nil, nil, fmt.Errorf("having column %q is not a selected aggregate alias", filter.Column)
				}
				compiled, err := compileFilterExpression(expression, filter)
				if err != nil {
					return "", nil, nil, nil, err
				}
				havings = append(havings, compiled)
			}
			b.WriteString(" HAVING ")
			b.WriteString(strings.Join(havings, " AND "))
		}
	} else if len(plan.Having) > 0 {
		return "", nil, nil, nil, errors.New("having requires aggregates")
	}
	return b.String(), internal, visible, aliases, nil
}

// canonicalAggregateResultEncoding is deliberately not PostgreSQL's general
// cast graph. It admits one named wire representation for an exact NUMERIC
// SUM; identity casts are erased by SQL lowering before QueryPlan.
func canonicalAggregateResultEncoding(function, naturalType, requested string) (string, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		return "", nil
	}
	if strings.ToLower(strings.TrimSpace(function)) == "sum" && naturalType == "numeric" && requested == NumericTextResultEncoding {
		return NumericTextResultEncoding, nil
	}
	return "", fmt.Errorf("aggregate result encoding %q for %s returning %q is outside the relational profile",
		requested, function, naturalType)
}

func hasAggregateResultEncoding(aggregates []Aggregate) bool {
	for _, aggregate := range aggregates {
		if strings.TrimSpace(aggregate.ResultEncoding) != "" {
			return true
		}
	}
	return false
}

// compileRelationalVisibleOrder admits the one closed ordering form needed by
// a grouped relational result: every group key must be selected and ordered
// exactly once. With no LIMIT/OFFSET, this changes delivery order only; the
// provenance companion retains its independent canonical group/member order.
func compileRelationalVisibleOrder(plan QueryPlan, fields map[string]relationalField, aliases map[string]string) (string, error) {
	if len(plan.OrderBy) == 0 {
		return "", nil
	}
	if len(plan.GroupBy) == 0 || len(plan.OrderBy) != len(plan.GroupBy) {
		return "", errors.New("relational ORDER BY requires every grouped key exactly once")
	}
	selected := make(map[string]struct{}, len(plan.Columns))
	for _, field := range plan.Columns {
		selected[field] = struct{}{}
	}
	groups := make(map[string]struct{}, len(plan.GroupBy))
	for _, field := range plan.GroupBy {
		if _, present := fields[field]; !present {
			return "", fmt.Errorf("group field %q is absent", field)
		}
		groups[field] = struct{}{}
	}
	seen := make(map[string]struct{}, len(plan.OrderBy))
	parts := make([]string, 0, len(plan.OrderBy))
	for _, order := range plan.OrderBy {
		if _, grouped := groups[order.Column]; !grouped {
			return "", fmt.Errorf("relational ORDER BY field %q is not a group key", order.Column)
		}
		if _, projected := selected[order.Column]; !projected {
			return "", fmt.Errorf("relational ORDER BY field %q is not selected", order.Column)
		}
		if _, duplicate := seen[order.Column]; duplicate {
			return "", fmt.Errorf("relational ORDER BY repeats %q", order.Column)
		}
		seen[order.Column] = struct{}{}
		direction := strings.ToUpper(strings.TrimSpace(order.Direction))
		if direction == "" {
			direction = "ASC"
		}
		if direction != "ASC" && direction != "DESC" {
			return "", errors.New("relational ORDER BY direction must be ASC or DESC")
		}
		alias := aliases[order.Column]
		if alias == "" {
			return "", fmt.Errorf("relational ORDER BY field %q has no output alias", order.Column)
		}
		parts = append(parts, quoteIdentifier(alias)+" "+direction)
	}
	return " ORDER BY " + strings.Join(parts, ", "), nil
}

// RelationalOutputAlias is the opaque output alias a relational compilation
// gives the projected column id (role.column). It is exported for the
// retrieval preparation, which wraps a compiled statement and must name its
// columns without parsing the SQL text.
func RelationalOutputAlias(id string) string { return relationalOutputAlias(id) }

func relationalOutputAlias(id string) string {
	digest := sha256.Sum256([]byte("taskgate-relational-output\x00" + id))
	return "tg_o_" + hex.EncodeToString(digest[:8])
}

func countFromMembers(from From) int {
	n := 0
	if from.Scan != nil {
		n++
	}
	if from.Join != nil {
		n++
	}
	if from.JoinMany != nil {
		n++
	}
	if from.UnionDistinct != nil {
		n++
	}
	return n
}
func safeRole(role string) bool {
	return len(role) <= 63 && safeIdentifier(role) && !strings.HasPrefix(role, "tg_")
}
func qualified(role, column string) string {
	return quoteIdentifier(role) + "." + quoteIdentifier(column)
}
func fieldSQL(field relationalField) string { return qualified(field.Role, field.Column) }

func fieldFromProduct(id, role, column string, product Product) relationalField {
	typeName, _ := canonicalProductType(product.ColumnTypes[column])
	return relationalField{ID: id, Role: role, Column: column, SQLType: typeName, Collation: product.ColumnCollations[column],
		CollationVersion: product.CollationVersions[column], Product: product.Name, AggregatePermissions: product.AllowedAggregates}
}
func canonicalProductType(value string) (string, error) {
	return exposure.CanonicalSQLTypeV2(value)
}

func compileScanFilters(scan Scan, product Product) (string, error) {
	return compileLeafFilterList(scan.Filters, scan.Role, product)
}
func compileLeafFilters(scan Scan, product Product) (string, error) {
	return compileLeafFilterList(scan.Filters, scan.Role, product)
}
func compileLeafFilterList(filters []Filter, role string, product Product) (string, error) {
	parts := make([]string, 0, len(filters))
	for _, filter := range filters {
		if err := allowedColumn(filter.Column, product); err != nil {
			return "", err
		}
		expression, err := compileFilterExpression(qualified(role, filter.Column), filter)
		if err != nil {
			return "", err
		}
		parts = append(parts, expression)
	}
	return strings.Join(parts, " AND "), nil
}
func compileQualifiedFilters(filters []Filter, fields map[string]relationalField) (string, error) {
	parts := make([]string, 0, len(filters))
	for _, filter := range filters {
		field, present := fields[filter.Column]
		if !present {
			return "", fmt.Errorf("filter field %q is absent", filter.Column)
		}
		expression, err := compileFilterExpression(fieldSQL(field), filter)
		if err != nil {
			return "", err
		}
		parts = append(parts, expression)
	}
	return strings.Join(parts, " AND "), nil
}
func compileFilterExpression(column string, filter Filter) (string, error) {
	op := strings.ToUpper(strings.TrimSpace(filter.Op))
	switch op {
	case "=", "!=", "<>", "<", "<=", ">", ">=", "LIKE":
		if op == "LIKE" {
			if _, ok := filter.Value.(string); !ok {
				return "", errors.New("LIKE requires a string literal")
			}
		}
		literal, err := sqlLiteral(filter.Value)
		if err != nil {
			return "", err
		}
		return column + " " + op + " " + literal, nil
	case "IN", "NOT IN":
		values, ok := filter.Value.([]any)
		if !ok || len(values) == 0 || len(values) > MaxRawPredicateLiterals {
			return "", fmt.Errorf("IN requires a non-empty JSON array of at most %d values", MaxRawPredicateLiterals)
		}
		literals := make([]string, 0, len(values))
		for _, value := range values {
			literal, err := sqlLiteral(value)
			if err != nil {
				return "", err
			}
			literals = append(literals, literal)
		}
		return column + " " + op + " (" + strings.Join(literals, ", ") + ")", nil
	default:
		return "", fmt.Errorf("filter operator %q is not allowed", filter.Op)
	}
}

func evidenceFields(scan Scan, product Product, schema map[string]relationalField, plan QueryPlan, joins []JoinPredicate, union []string) []string {
	set := make(map[string]struct{})
	for _, field := range product.StableEntityKey {
		set[field] = struct{}{}
	}
	for _, field := range product.RequiredEvidence {
		set[field] = struct{}{}
	}
	for _, filter := range scan.Filters {
		set[filter.Column] = struct{}{}
	}
	for _, filter := range plan.Filters {
		if field, ok := schema[filter.Column]; ok && field.Role == scan.Role {
			set[field.Column] = struct{}{}
		}
	}
	dependencyIDs := append(append([]string(nil), plan.Columns...), plan.GroupBy...)
	for _, aggregate := range plan.Aggregates {
		if aggregate.Column != "*" {
			dependencyIDs = append(dependencyIDs, aggregate.Column)
		}
	}
	for _, id := range dependencyIDs {
		if field, ok := schema[id]; ok && field.Role == scan.Role {
			set[field.Column] = struct{}{}
		}
	}
	for _, predicate := range joins {
		for _, id := range []string{predicate.Left, predicate.Right} {
			if field, ok := schema[id]; ok && field.Role == scan.Role {
				set[field.Column] = struct{}{}
			}
		}
	}
	for _, field := range union {
		set[field] = struct{}{}
	}
	return sortedColumns(set)
}
func evidenceAliases(role string, fields []string) map[string]string {
	result := make(map[string]string, len(fields))
	for _, field := range fields {
		result[field] = "tg_" + role + "_" + field
	}
	return result
}
func sortedColumns(set map[string]struct{}) []string {
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
func sortedRelationalFieldIDs(fields map[string]relationalField) []string {
	result := make([]string, 0, len(fields))
	for id := range fields {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
func uniqueOrdered(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
func splitFieldID(id string) (string, string, bool) {
	parts := strings.Split(id, ".")
	if len(parts) != 2 || !safeRole(parts[0]) || !safeIdentifier(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}
func cloneFilters(filters []Filter) []Filter { return append([]Filter(nil), filters...) }
func relationalProducts(sources []RelationalSource) []string {
	set := make(map[string]struct{})
	for _, source := range sources {
		set[source.Product] = struct{}{}
	}
	return sortedColumns(set)
}

// RelationalProductNames returns the logical products named by the public
// closed grammar without accepting or normalizing any SQL identifiers.
func RelationalProductNames(plan QueryPlan) ([]string, error) {
	if plan.From == nil || countFromMembers(*plan.From) != 1 {
		return nil, errors.New("relational QueryPlan has no unique from operator")
	}
	var names []string
	switch {
	case plan.From.Scan != nil:
		names = []string{plan.From.Scan.Product}
	case plan.From.Join != nil:
		names = []string{plan.From.Join.Left.Product, plan.From.Join.Right.Product}
	case plan.From.JoinMany != nil:
		for _, source := range plan.From.JoinMany.Sources {
			names = append(names, source.Product)
		}
	case plan.From.UnionDistinct != nil:
		names = []string{plan.From.UnionDistinct.Left.Product, plan.From.UnionDistinct.Right.Product}
	}
	for _, name := range names {
		if !safeIdentifier(name) {
			return nil, errors.New("relational QueryPlan has an invalid product")
		}
	}
	return sortedColumns(func() map[string]struct{} {
		set := make(map[string]struct{}, len(names))
		for _, name := range names {
			set[name] = struct{}{}
		}
		return set
	}()), nil
}
