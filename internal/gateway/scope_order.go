package gateway

import (
	"sort"
	"strings"
)

// Ordered scope dimensions (KB1-LOOP-152; the author's decision of
// 2026-10-28). A Catalog enum scope that declares order (classification:
// public < internal < secret < confidential < top_secret) is widened downward
// when a task is requested: asking for a grade means asking for every grade
// below it. request_data_task, request_download_task and
// issue_data_authorization all pass through createTask, which calls
// closeOrderedScopes right after scope normalization, so the eligibility gate,
// the approval route, delegation and the manifest all see the whole set, and
// the approver may still remove values. Nothing after the request reads the
// order: a grant admits exactly the values it lists, and receipts and audit
// say so. A Catalog without any order is untouched: no value is added and no
// response carries scope_closure.

// closeOrderedScopes replaces every requested value set of an ordered Catalog
// scope by its down-closure (catalog.Scope.DownClosure), sorted as
// normalizeScopes leaves enum sets. It returns the scope and, per scope, the
// values it added, lowest first; added is nil when nothing was added.
func (s *Service) closeOrderedScopes(scope map[string]any) (map[string]any, map[string][]string) {
	if s.catalog == nil {
		return scope, nil
	}
	var (
		closed map[string]any
		added  map[string][]string
	)
	for _, definition := range s.catalog.Scopes {
		if len(definition.Order) == 0 {
			continue
		}
		raw, present := scope[definition.Name]
		if !present {
			continue
		}
		requested, ok := scopeStringValues(raw)
		if !ok {
			continue
		}
		have := make(map[string]bool, len(requested))
		for _, value := range requested {
			have[value] = true
		}
		closure := definition.DownClosure(requested)
		var extra []string
		for _, value := range closure {
			if !have[value] {
				extra = append(extra, value)
			}
		}
		if len(extra) == 0 {
			continue
		}
		if closed == nil {
			closed = make(map[string]any, len(scope))
			for name, value := range scope {
				closed[name] = value
			}
			added = make(map[string][]string)
		}
		sort.Strings(closure)
		if _, isAny := raw.([]any); isAny {
			values := make([]any, 0, len(closure))
			for _, value := range closure {
				values = append(values, value)
			}
			closed[definition.Name] = values
		} else {
			closed[definition.Name] = closure
		}
		added[definition.Name] = extra
	}
	if closed == nil {
		return scope, nil
	}
	return closed, added
}

// orderedScopesDescription is what the task-request tools add to their
// description when the Catalog declares ordered scopes, read from the
// Catalog: one sentence per ordered scope, in declaration order. It is ""
// for a Catalog without any, whose tool descriptions stay as they are.
func (s *Service) orderedScopesDescription() string {
	if s.catalog == nil {
		return ""
	}
	var text strings.Builder
	for _, definition := range s.catalog.Scopes {
		if len(definition.Order) > 0 {
			text.WriteString(definition.Name + " 是有序维度（" + strings.Join(definition.Order, " < ") +
				"）：填最高一级即可，低于它的各级自动包含（返回 scope_closure 列出补进的值）；审批人可删减。")
		}
	}
	return text.String()
}

// scopeOrderOf maps each of the named scopes that the Catalog orders to its
// order, for describe_data_product; nil when none is ordered.
func (s *Service) scopeOrderOf(names []string) map[string][]string {
	if s.catalog == nil {
		return nil
	}
	var orders map[string][]string
	for _, name := range names {
		if order := s.catalog.ScopeOrder(name); order != nil {
			if orders == nil {
				orders = make(map[string][]string)
			}
			orders[name] = order
		}
	}
	return orders
}

// orderedScopeTools are the tools whose description names the ordered scopes.
var orderedScopeTools = map[string]bool{"request_data_task": true, "request_download_task": true, "issue_data_authorization": true}
