package gateway

import (
	"encoding/json"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// Knowledge products on the discovery surface (KB1-LOOP-137).
//
// list_data_products and describe_data_product publish what a requester needs
// to use a knowledge product without learning its rules by refusal: its name
// and relations, how a query is formed and bounded, the attribute vocabulary,
// and what a task must approve for each tool. Each requirement is read off the
// code that enforces it -- the retrieval plan (RetrievalRequiredColumns), the
// normalizer's operator table and the aggregate plan builder -- so this page
// cannot promise what the tools refuse. The physical names (collection,
// source namespace, embedding weights) and the eligibility rules stay private.

func (s *Service) publicKnowledgeProduct(kp catalog.KnowledgeProduct) map[string]any {
	relations := map[string]any{}
	for _, relation := range knowledgeRelationRoles(kp) {
		relations[relation.role] = relation.product
	}
	return map[string]any{
		"name": kp.Name, "kind": "knowledge_product", "description": kp.Description,
		"sensitivity": kp.Sensitivity, "scopes": append([]string{}, kp.Scopes...),
		"epoch": kp.IndexPublication, "document_types": append([]string{}, kp.DocumentTypes...),
		"relations":         relations,
		"retrieval":         s.publicRetrieval(kp),
		"attributes":        s.publicKnowledgeAttributes(kp),
		"extracted_fields":  s.publicExtractedFields(kp),
		"task_requirements": s.knowledgeTaskRequirements(kp),
	}
}

func (s *Service) publicRetrieval(kp catalog.KnowledgeProduct) map[string]any {
	return map[string]any{
		"k_max": kp.Retrieval.KMax, "exact": kp.Retrieval.Exact, "keyword": kp.Retrieval.Keyword,
		"query_rule": "query 只能是 text、vector、keywords 三者之一",
		"query_modes": []map[string]any{
			{"mode": "text", "available": s.kbEmbedder != nil, "max_bytes_after_normalization": retrieval.MaxQueryTextBytes},
			{"mode": "vector", "available": true, "model": kp.Embedding.Model, "dimension": kp.Embedding.Dimension},
			{"mode": "keywords", "available": kp.Retrieval.Keyword, "max_keywords": retrieval.MaxKeywords,
				"max_keyword_chars": retrieval.MaxKeywordRunes, "match": "正文同时包含每个关键词（区分大小写），按命中字符数排序"},
		},
		"result_columns": append([]string{}, retrievalDisplayColumns...),
		"ranking":        "结果按相似度（关键词检索按命中字符数）排序，名次即返回顺序；按设计不返回分数",
		"filters":        "只接受 attributes 中声明的属性，不接受 doc_id；要整份文档用 request_download_task",
	}
}

// publicKnowledgeAttributes lists the filter vocabulary. A scoped attribute's
// values are its scope's; the task's mandatory scope is applied to every
// search, so a caller filter on it must stay inside the task's values.
func (s *Service) publicKnowledgeAttributes(kp catalog.KnowledgeProduct) []map[string]any {
	scopes := map[string]catalog.Scope{}
	for _, scope := range s.catalog.Scopes {
		scopes[scope.Name] = scope
	}
	base, _ := physicalquery.RetrievalRequiredColumns(kp)
	attributes := make([]map[string]any, 0, len(kp.Attributes))
	for _, attribute := range kp.Attributes {
		public := map[string]any{"name": attribute.Name, "type": attribute.Type}
		allowed := attribute.AllowedValues
		if attribute.Scope != "" {
			public["scope"] = attribute.Scope
			public["scope_note"] = "任务强制范围会自动施加在每次检索上；对它的过滤必须落在任务范围内"
			if len(allowed) == 0 {
				allowed = scopes[attribute.Scope].AllowedValues
			}
		}
		if len(allowed) > 0 {
			public["allowed_values"] = append([]string{}, allowed...)
		}
		operators := []string{}
		for _, operator := range retrieval.AdmittedOperators(attribute.Type) {
			if physicalquery.RetrievalOperatorRendered(operator) {
				operators = append(operators, operator)
			}
		}
		public["search_operators"] = operators
		if len(operators) == 0 {
			public["search_note"] = "该类型的过滤暂不能执行，不能用于检索过滤"
		} else if filtered, err := physicalquery.RetrievalRequiredColumns(kp, attribute.Name); err == nil {
			public["filter_columns"] = columnsBeyond(filtered, base)
		}
		attributes = append(attributes, public)
	}
	return attributes
}

// publicExtractedFields lists the extracted fields and, per field, the
// aggregate functions the aggregate tool admits and the columns its plan
// reads. Both come from aggregatePlan itself, run against a task that has
// approved the whole knowledge product, and from the value relation's
// allowed aggregates the compiler checks.
func (s *Service) publicExtractedFields(kp catalog.KnowledgeProduct) []map[string]any {
	fields := make([]map[string]any, 0, len(kp.ExtractionRules))
	seen := map[string]bool{}
	for _, rule := range kp.ExtractionRules {
		if seen[rule.Field] {
			continue
		}
		seen[rule.Field] = true
		public := map[string]any{"field": rule.Field, "type": rule.Type, "document_type": rule.DocumentType, "scope": rule.Scope}
		functions, relation, columns := s.aggregateRequirements(kp, rule.Field)
		public["aggregates"] = functions
		if relation != "" {
			public["relation"], public["columns"] = relation, columns
		}
		fields = append(fields, public)
	}
	return fields
}

// aggregateRequirements runs aggregatePlan for every function on one field
// and keeps the ones whose plan the value relation's allowed aggregates admit.
func (s *Service) aggregateRequirements(kp catalog.KnowledgeProduct, field string) ([]string, string, []string) {
	scope := map[string]any{}
	for _, name := range kp.Scopes {
		for _, declared := range s.catalog.Scopes {
			if declared.Name == name {
				scope[name] = declared.AllowedValues
			}
		}
	}
	mandatory, err := json.Marshal(scope)
	if err != nil {
		return []string{}, "", nil
	}
	grant := control.TaskGrant{ApprovedProducts: kp.Products.Relations(), MandatoryScope: mandatory}
	functions := []string{}
	relation := ""
	columns := map[string]struct{}{}
	for _, function := range []string{"count", "sum", "min", "max"} {
		body, err := json.Marshal(map[string]any{"knowledge_product": kp.Name, "field": field, "aggregate": function})
		if err != nil {
			continue
		}
		request, err := retrieval.DecodeRequest(retrieval.ToolAggregate, body)
		if err != nil {
			continue
		}
		plan, err := s.aggregatePlan(grant, request)
		if err != nil {
			continue
		}
		product, found := s.catalog.LookupProduct(plan.Product)
		if !found || !containsFold(product.AllowedAggregates, function) {
			continue
		}
		functions = append(functions, function)
		relation = plan.Product
		for _, column := range singleProductPlanColumns(plan)[plan.Product] {
			columns[column] = struct{}{}
		}
	}
	return functions, relation, sortedColumnSet(columns)
}

// knowledgeTaskRequirements says, per tool, which relations and columns a
// task must approve. The search and fetch columns are the retrieval plan's;
// every filtered attribute adds its filter_columns.
func (s *Service) knowledgeTaskRequirements(kp catalog.KnowledgeProduct) map[string]any {
	base, _ := physicalquery.RetrievalRequiredColumns(kp)
	retrievalProducts := []string{kp.Products.Chunks, kp.Products.Documents}
	requirements := map[string]any{
		"search": map[string]any{"products": retrievalProducts, "columns": base,
			"per_filter":       "每个要过滤的属性另需该属性的 filter_columns（见 attributes）",
			"exposure_profile": exposure.ProfileV5, "knowledge_product_argument": kp.Name},
		"fetch": map[string]any{"products": retrievalProducts, "columns": base,
			"also": "只能取本任务族此前 search/fetch 返回过的块", "knowledge_product_argument": kp.Name},
		"download": map[string]any{"tool": "request_download_task",
			"note": "整份文档或原件请申请下载任务（每份文档逐份审批），产品与列由网关固定，不用自己填"},
		"note": "授权只能收窄：缺关系或缺列不能在原任务上补，需按此处新建任务；申请时 request_data_task 会预告 knowledge_readiness",
	}
	aggregate := map[string]any{}
	for _, field := range s.publicExtractedFields(kp) {
		if functions, _ := field["aggregates"].([]string); len(functions) > 0 {
			aggregate[field["field"].(string)] = map[string]any{"aggregates": functions,
				"products": []any{field["relation"]}, "columns": field["columns"]}
		}
	}
	if len(aggregate) > 0 {
		requirements["aggregate"] = aggregate
	}
	return requirements
}

// knowledgeReadiness says, for every knowledge product the given relations
// touch, which knowledge tools a task approving exactly those relations and
// columns could use, and what is missing. It is information only: it never
// refuses, and the approver may still narrow the request. An unknown exposure
// profile skips that requirement.
func (s *Service) knowledgeReadiness(products []string, columns map[string][]string, profile, basis string) []map[string]any {
	if s.catalog == nil {
		return nil
	}
	readiness := []map[string]any{}
	for _, kp := range s.catalog.KnowledgeProducts {
		touched := false
		for _, relation := range kp.Products.Relations() {
			if relation != "" && contains(products, relation) {
				touched = true
			}
		}
		if !touched {
			continue
		}
		entry := map[string]any{"knowledge_product": kp.Name, "basis": basis}
		if basis == "requested" {
			entry["note"] = "按申请清单预告；审批人可能删减产品或列"
		}
		retrievalProducts := []string{kp.Products.Chunks, kp.Products.Documents}
		base, err := physicalquery.RetrievalRequiredColumns(kp)
		if err == nil {
			search := map[string]any{}
			missingProducts := []string{}
			for _, relation := range retrievalProducts {
				if !contains(products, relation) {
					missingProducts = append(missingProducts, relation)
				}
			}
			missingColumns := missingPlanColumns(base, columns)
			for _, relation := range missingProducts {
				delete(missingColumns, relation)
			}
			profileOK := profile == "" || profile == exposure.ProfileV5
			search["ready"] = len(missingProducts) == 0 && len(missingColumns) == 0 && profileOK
			if len(missingProducts) > 0 {
				search["missing_products"] = missingProducts
			}
			if len(missingColumns) > 0 {
				search["missing_columns"] = missingColumns
			}
			if !profileOK {
				search["exposure_profile"] = profile
				search["requires_exposure_profile"] = exposure.ProfileV5
			}
			// An attribute is filterable when the columns its filter adds are
			// approved; an attribute without a plan rendering never is.
			filterable, unfilterable := []string{}, []string{}
			for _, attribute := range kp.Attributes {
				filtered, err := physicalquery.RetrievalRequiredColumns(kp, attribute.Name)
				approved := err == nil
				for _, reference := range columnsBeyond(filtered, base) {
					relation, column, _ := strings.Cut(reference, ".")
					approved = approved && contains(columns[relation], column)
				}
				if approved {
					filterable = append(filterable, attribute.Name)
				} else {
					unfilterable = append(unfilterable, attribute.Name)
				}
			}
			search["filterable_attributes"], search["unfilterable_attributes"] = filterable, unfilterable
			entry["search"], entry["fetch"] = search, search
		}
		aggregate := map[string]any{}
		for _, field := range s.publicExtractedFields(kp) {
			functions, _ := field["aggregates"].([]string)
			relation, _ := field["relation"].(string)
			if len(functions) == 0 || relation == "" {
				continue
			}
			required, _ := field["columns"].([]string)
			status := map[string]any{"ready": contains(products, relation)}
			if !contains(products, relation) {
				status["missing_products"] = []string{relation}
			} else if missing := missingPlanColumns(map[string][]string{relation: required}, columns); len(missing) > 0 {
				status["ready"], status["missing_columns"] = false, missing
			}
			aggregate[field["field"].(string)] = status
		}
		if len(aggregate) > 0 {
			entry["aggregate"] = aggregate
		}
		readiness = append(readiness, entry)
	}
	return readiness
}

// columnsBeyond lists, as "relation.column", what extended adds to base.
func columnsBeyond(extended, base map[string][]string) []string {
	out := []string{}
	for relation, columns := range extended {
		for _, column := range columns {
			if !contains(base[relation], column) {
				out = append(out, relation+"."+column)
			}
		}
	}
	sort.Strings(out)
	return out
}

func sortedColumnSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for column := range set {
		out = append(out, column)
	}
	sort.Strings(out)
	return out
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), wanted) {
			return true
		}
	}
	return false
}
