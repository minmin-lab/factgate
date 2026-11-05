package gateway

import (
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// Precise refusals for the knowledge tools (KB1-LOOP-137). A refusal names
// what the request or the task lacks, and what to do, in its details, so an
// agent need not learn the retrieval rules by trial. Every check here reads
// the same plan the preparation compiles, and none of them admits anything
// the preparation would refuse: they only refuse earlier and say why.

// knowledgeRemedy is the one remedy for a task that lacks something: a grant
// only ever narrows, delegation included, so the fix is a new task.
const knowledgeRemedy = "现任务的授权不能扩大（委托也只能收窄）；请按 list_data_products 中该知识产品的 task_requirements 新建任务"

// unknownKnowledgeProduct refuses a knowledge_product argument the Catalog
// does not declare. It is a name lookup, not a grant check, so it is an
// invalid request, as request_download_task always reported it.
func (s *Service) unknownKnowledgeProduct(name string) error {
	names := make([]string, 0)
	if s.catalog != nil {
		for _, kp := range s.catalog.KnowledgeProducts {
			names = append(names, kp.Name)
		}
	}
	sort.Strings(names)
	details := map[string]any{"reason": "UNKNOWN_KNOWLEDGE_PRODUCT", "knowledge_product": name, "knowledge_products": names}
	message := fmt.Sprintf("目录中没有名为 %q 的知识产品；knowledge_product 应填知识产品名，可用的见 knowledge_products", name)
	if owner, role, found := s.knowledgeRelationOf(name); found {
		details["suggested_knowledge_product"], details["relation_role"] = owner.Name, role
		message = fmt.Sprintf("%q 是知识产品 %s 的 %s 关系，不是知识产品；knowledge_product 请填 %s", name, owner.Name, role, owner.Name)
	}
	return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: message, Details: details}
}

// knowledgeRelationsNotGranted refuses a task that lacks a relation the tool
// reads, naming the missing ones.
func knowledgeRelationsNotGranted(message string, kp catalog.KnowledgeProduct, tool string, required, approved []string) error {
	missing := make([]string, 0, len(required))
	for _, relation := range required {
		if !contains(approved, relation) {
			missing = append(missing, relation)
		}
	}
	return &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: message + "（缺 " + strings.Join(missing, "、") + "）",
		Details: map[string]any{"reason": "RELATIONS_NOT_GRANTED", "knowledge_product": kp.Name, "tool": tool,
			"required_products": required, "missing_products": missing, "remedy": knowledgeRemedy}}
}

// missingPlanColumns is the part of required the task has not approved, per
// relation, sorted.
func missingPlanColumns(required, approved map[string][]string) map[string][]string {
	missing := map[string][]string{}
	for relation, columns := range required {
		for _, column := range columns {
			if !contains(approved[relation], column) {
				missing[relation] = append(missing[relation], column)
			}
		}
	}
	for relation := range missing {
		sort.Strings(missing[relation])
	}
	return missing
}

// columnsNotGranted refuses a plan that reads columns the task has not
// approved. It is the refusal the compiler would make anyway; it only names
// the columns.
func columnsNotGranted(kp catalog.KnowledgeProduct, tool string, missing map[string][]string) error {
	relations := make([]string, 0, len(missing))
	for relation := range missing {
		relations = append(relations, relation)
	}
	sort.Strings(relations)
	parts := make([]string, 0, len(relations))
	for _, relation := range relations {
		parts = append(parts, relation+"."+strings.Join(missing[relation], "、"+relation+"."))
	}
	return &mcp.ToolError{Code: apierr.CodePolicyDenied,
		Message: fmt.Sprintf("%s 读取的列未获任务授权：%s", tool, strings.Join(parts, "、")),
		Details: map[string]any{"reason": "COLUMNS_NOT_GRANTED", "knowledge_product": kp.Name, "tool": tool,
			"missing_columns": missing, "remedy": knowledgeRemedy}}
}

// requireRenderedOperators refuses a caller atom whose operator the language
// admits but the retrieval plan cannot render yet (text_set "has"). The
// preparation refuses it too; this says so instead of the generic refusal.
func requireRenderedOperators(form retrieval.NormalForm) error {
	for _, atom := range form.CallerAtoms() {
		if !physicalquery.RetrievalOperatorRendered(atom.Operator) {
			return &mcp.ToolError{Code: apierr.CodeRetrievalFilterUnsupported,
				Message: fmt.Sprintf("属性 %s（%s）上的 %q 过滤暂不能执行；该属性目前不能用于检索过滤", atom.Attribute, atom.Type, atom.Operator),
				Details: map[string]any{"reason": "OPERATOR_NOT_EXECUTABLE", "attribute": atom.Attribute,
					"attribute_type": atom.Type, "operator": atom.Operator}}
		}
	}
	return nil
}

// requireRetrievalColumns refuses a search or fetch whose equivalent plan
// reads a column the task has not approved. A plan the columns cannot be
// read off is left to the preparation, which refuses it closed.
func requireRetrievalColumns(kp catalog.KnowledgeProduct, form retrieval.NormalForm, approved map[string][]string) error {
	required, err := physicalquery.RetrievalPlanColumns(kp, form)
	if err != nil {
		return nil
	}
	if missing := missingPlanColumns(required, approved); len(missing) > 0 {
		return columnsNotGranted(kp, form.Tool, missing)
	}
	return nil
}

// singleProductPlanColumns names the columns a single-product plan reads:
// projection, filters, aggregate inputs and grouping keys.
func singleProductPlanColumns(plan queryplan.QueryPlan) map[string][]string {
	set := map[string]struct{}{}
	for _, column := range plan.Columns {
		set[column] = struct{}{}
	}
	for _, filter := range plan.Filters {
		set[filter.Column] = struct{}{}
	}
	for _, aggregate := range plan.Aggregates {
		if aggregate.Column != "" {
			set[aggregate.Column] = struct{}{}
		}
	}
	for _, column := range plan.GroupBy {
		set[column] = struct{}{}
	}
	columns := make([]string, 0, len(set))
	for column := range set {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	return map[string][]string{plan.Product: columns}
}
