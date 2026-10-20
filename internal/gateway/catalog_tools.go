package gateway

import (
	"context"
	"encoding/json"
	"strings"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

const catalogReportingSQLProfile = sqllowering.Profile

func (s *Service) describeDataProduct(_ context.Context, _ mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	args.Name = strings.TrimSpace(args.Name)
	if args.Name == "" {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "name 必须是非空逻辑数据产品名"}
	}
	product, found := s.catalog.LookupProduct(args.Name)
	if !found {
		// A knowledge product is named like a Product but describes a
		// collection; the Catalog forbids the two namespaces to overlap.
		if kp, isKnowledge := s.catalog.LookupKnowledgeProduct(args.Name); isKnowledge {
			result := s.publicKnowledgeProduct(kp)
			result["catalog_version"] = s.catalog.CatalogVersion
			return result, nil
		}
		return nil, &mcp.ToolError{Code: apierr.CodeNotFound, Message: "请求的数据产品不存在"}
	}
	result, err := publicDataProduct(product, true)
	if err != nil {
		return nil, err
	}
	if kp, role, isRelation := s.knowledgeRelationOf(product.Name); isRelation {
		result["knowledge_product"] = map[string]any{"name": kp.Name, "role": role}
	}
	result["catalog_version"] = s.catalog.CatalogVersion
	return result, nil
}

func (s *Service) getSQLCapabilities(_ context.Context, _ mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct{}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	rewriteErrorCodes := []string{
		apierr.CodeSQLSyntaxError,
		apierr.CodeProductNotApproved,
		apierr.CodeColumnNotApproved,
		apierr.CodeSQLNotLowerable,
		apierr.CodeJoinTypeUnsupported,
		apierr.CodeJoinGraphDisconnected,
		apierr.CodeJoinKeyTypeMismatch,
		apierr.CodeCollationMismatch,
		apierr.CodeSubqueryUnsupported,
		apierr.CodeViewQueryUnsupported,
	}
	return map[string]any{
		"catalog_version":            s.catalog.CatalogVersion,
		"sql_profile":                catalogReportingSQLProfile,
		"statement_types":            []string{"SELECT"},
		"single_statement_only":      true,
		"logical_products_only":      true,
		"lossless_lowering_required": true,
		"join": map[string]any{
			"types":              []string{"INNER"},
			"predicate":          "equality",
			"graph":              "connected",
			"min_sources":        2,
			"max_sources":        queryplan.MaxJoinSources,
			"limit_kind":         "operational_complexity_guard",
			"arbitrary_topology": true,
			"self_join":          false,
		},
		"pagination": map[string]any{
			"single_product": true,
			"joined_query":   false,
		},
		"ordering": map[string]any{
			"single_product":                         true,
			"joined_grouped_complete_selected_key":   true,
			"joined_ungrouped":                       false,
			"joined_partial_or_unselected_group_key": false,
			"joined_aggregate_expression":            false,
		},
		"projection_casts": map[string]any{
			"identity_scalar": []string{"bigint", "int8", "numeric", "text"},
			"joined_ordered_grouped_numeric_sum_to_wire_text": queryplan.NumericTextResultEncoding,
			"general_expression_casts":                        false,
		},
		"semantic_views": map[string]any{
			"profile_version":                    catalog.ViewContractV1,
			"nested_dag":                         true,
			"max_expanded_sources":               queryplan.MaxJoinSources,
			"max_depth":                          viewcompiler.MaxViewDepth,
			"max_nodes":                          viewcompiler.MaxViewNodes,
			"max_dependency_edges":               viewcompiler.MaxDependencyEdges,
			"max_predicates":                     viewcompiler.MaxPredicates,
			"max_definition_bytes":               viewcompiler.MaxDefinitionBytes,
			"terminal_relations":                 []string{"governed_materialized_view"},
			"join_types":                         []string{"INNER"},
			"join_predicate":                     "equality",
			"aggregate_functions":                []string{"count", "sum", "min", "max"},
			"aggregate_barriers_max":             1,
			"aggregate_barrier_above":            "projection_only",
			"query_time_join_with_other_product": false,
			"order_by":                           false,
			"limit":                              false,
			"offset":                             false,
			"exposure_required":                  true,
			"shared_child_self_join":             false,
			"rebind_on_drift":                    true,
		},
		"catalog_controls": map[string]any{
			"columns":    "per_product",
			"aggregates": "per_product",
			"operators":  "per_product",
			"functions":  "per_product",
		},
		"rewrite_error_codes": rewriteErrorCodes,
		"rebind_error_codes":  []string{apierr.CodeViewSemanticChanged},
		// Backward-compatible alias for clients predating the split between
		// query rewrites and task rebinds. New clients should use the two
		// explicit fields above; semantic drift is intentionally not included.
		"repairable_error_codes": append([]string(nil), rewriteErrorCodes...),
		"features": map[string]any{
			"projection":                            true,
			"filters":                               true,
			"group_by":                              true,
			"aggregates":                            true,
			"order_by":                              true,
			"limit":                                 true,
			"offset":                                true,
			"inner_equijoins":                       true,
			"multi_relation_join_graphs":            true,
			"multiple_equality_predicates_per_edge": true,
			"multiple_statements":                   false,
			"data_modification":                     false,
			"outer_joins":                           false,
			"cross_joins":                           false,
			"non_equality_joins":                    false,
			"disconnected_join_graphs":              false,
			"subqueries":                            false,
			"common_table_expressions":              false,
			"set_operations":                        false,
			"window_functions":                      false,
			"having":                                true,
			"count_distinct":                        true,
			"avg_exact_numeric":                     true,
			"select_distinct":                       false,
			"implicit_star_projection":              true,
			"positional_order_group_refs":           false,
		},
	}, nil
}

func publicDataProduct(product catalog.Product, detailed bool) (map[string]any, error) {
	sensitivity, err := product.EffectiveSensitivity()
	if err != nil {
		return nil, err
	}
	fields := make([]map[string]any, 0, len(product.Fields))
	for _, field := range product.Fields {
		fieldSensitivity := field.Sensitivity
		if fieldSensitivity == "" {
			fieldSensitivity = product.Sensitivity
		}
		publicField := map[string]any{
			"name": field.Name, "type": field.Type, "description": field.Description,
			"sensitivity": fieldSensitivity,
		}
		if field.Collation != "" {
			publicField["collation"] = field.Collation
			publicField["collation_version"] = field.CollationVersion
		}
		fields = append(fields, publicField)
	}
	result := map[string]any{
		"name":                 product.Name,
		"description":          product.Description,
		"sensitivity":          sensitivity,
		"fields":               fields,
		"scopes":               append([]string{}, product.Scopes...),
		"snapshot":             product.Snapshot,
		"entity_key":           append([]string{}, product.EntityKey...),
		"stable_relation_role": product.StableRelationRole,
		"allowed_aggregates":   append([]string{}, product.AllowedAggregates...),
		"sql_profile":          catalogReportingSQLProfile,
	}
	if product.ViewContract != nil {
		result["semantic_view"] = map[string]any{
			"profile_version": product.ViewContract.ProfileVersion,
			"nested_dag":      true,
			"inner_equijoin":  true,
		}
		if detailed {
			result["view_contract"] = *product.ViewContract
		}
	}
	if detailed {
		result["allowed_operators"] = append([]string{}, product.AllowedOperators...)
		result["allowed_functions"] = append([]string{}, product.AllowedFunctions...)
	}
	return result, nil
}
