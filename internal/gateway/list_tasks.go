package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
)

// Listing one's tasks under the delegated-authorization model (KB1-LOOP-150;
// the demo, not a paper claim). With the Catalog's delegated_authorization
// block, list_my_tasks says what each task is -- a project or a day task (root
// tasks), a child task inside a project, or an authorization a department
// administrator issued -- and what it covers, so an agent can answer "which projects do I
// have". A root task is a project when its budget profile was chosen at
// creation and a day task when it took the route's default (author,
// 2026-10-28). Without the block every entry stays publicTask.

const (
	taskKindProject       = "project"
	taskKindTask          = "task"
	taskKindChild         = "child"
	taskKindAuthorization = "authorization"
	// taskKindRoot is a root task recorded before KB1-LOOP-150: whether its
	// budget profile was chosen is unknown, so it is neither project nor task.
	taskKindRoot = "root"
)

func validTaskKind(kind string) bool {
	switch kind {
	case taskKindProject, taskKindTask, taskKindChild, taskKindAuthorization, taskKindRoot:
		return true
	}
	return false
}

// listedTask is publicTask plus the task's kind, budget profile, products,
// scopes and approval roles, read from its private request context. A context
// that does not decode leaves publicTask marked detail_unavailable (and no
// kind) rather than failing the listing.
func (s *Service) listedTask(ctx context.Context, task control.Task) (map[string]any, string, error) {
	item := publicTask(task)
	persisted, err := decodePersistedPending(task)
	if err != nil {
		item["detail_unavailable"] = true
		return item, "", nil
	}
	pending := persisted.pendingContext
	var kind string
	switch {
	case pending.IssuedBy != nil:
		kind = taskKindAuthorization
	case task.ParentTaskID != "":
		kind = taskKindChild
	case pending.BudgetProfile == "":
		kind = taskKindRoot
	case pending.BudgetProfileChosen:
		kind = taskKindProject
	default:
		kind = taskKindTask
	}
	item["kind"] = kind
	// The profile recorded at creation (a child's budget is that profile
	// intersected with its parent's grant); for a record written before it
	// was recorded, the profile whose limits the budget equals.
	if name := pending.BudgetProfile; name != "" {
		item["budget_profile"] = name
	} else if name := s.budgetProfileName(pending.Budget); name != "" {
		item["budget_profile"] = name
	}
	item["products"] = append([]string(nil), pending.Products...)
	var scopes any = pending.MandatoryScope
	if task.State == control.TaskActive || task.State == control.TaskArchived {
		// An approver may have narrowed the scope: the grant holds what was
		// approved. A task archived before any approval has no grant.
		grant, grantErr := s.store.GetGrant(ctx, task.ID)
		switch {
		case grantErr == nil:
			var approved any
			if err := json.Unmarshal(grant.MandatoryScope, &approved); err != nil {
				return nil, "", err
			}
			scopes = approved
			// Likewise the products: an approver may have dropped some.
			item["products"] = append([]string(nil), grant.ApprovedProducts...)
		case errors.Is(grantErr, control.ErrNotFound):
		default:
			return nil, "", grantErr
		}
	}
	item["scopes"] = scopes
	approver := pending.Approver
	if pending.Chain != nil && task.State == control.TaskAwaitingApproval {
		// The chain's current step, as get_task_status reports it; Approver
		// is its first step. One approval-event read per such task.
		if progress, progressErr := s.chainProgressOf(ctx, task, persisted); progressErr == nil && progress.Next != nil {
			approver = progress.Next.Approver
		}
	}
	item["approver"] = approver
	item["self_confirm"], item["member_confirm"], item["agent_submitted"] = pending.SelfConfirm, pending.MemberConfirm, pending.AgentSubmitted
	if pending.IssuedBy != nil {
		item["issued_by"], item["human_subject"] = pending.IssuedBy.Subject, persisted.Manifest.HumanSubject
	}
	return item, kind, nil
}

// budgetVocabulary is what the delegated tool descriptions say about the
// Catalog's budget profiles (KB1-LOOP-150), read from the Catalog, never
// fixed: the routes' defaults, which a root task without budget_profile
// takes (a day task), and the other profiles, which a project chooses; each
// "name（TTL）".
type budgetVocabulary struct {
	defaults, chosen []string
}

func budgetVocabularyOf(c *catalog.Catalog) budgetVocabulary {
	routeDefault := map[string]bool{}
	for _, route := range c.ApprovalRoutes {
		routeDefault[route.BudgetProfile] = true
	}
	var vocabulary budgetVocabulary
	for _, profile := range c.BudgetProfiles {
		label := profile.Name + "（" + ttlText(profile.TaskTTL.Duration) + "）"
		if routeDefault[profile.Name] {
			vocabulary.defaults = append(vocabulary.defaults, label)
		} else {
			vocabulary.chosen = append(vocabulary.chosen, label)
		}
	}
	return vocabulary
}

// chosenExample is "（本目录路由缺省之外的预算档：…）", or "" when every
// profile is some route's default.
func (v budgetVocabulary) chosenExample() string {
	if len(v.chosen) == 0 {
		return ""
	}
	return "（本目录路由缺省之外的预算档：" + strings.Join(v.chosen, "、") + "）"
}

// defaultList is "（本目录：…）" naming the routes' default profiles.
func (v budgetVocabulary) defaultList() string {
	if len(v.defaults) == 0 {
		return ""
	}
	return "（本目录：" + strings.Join(v.defaults, "、") + "）"
}

// ttlText says a task TTL in days, hours or minutes when it is a whole number
// of them.
func ttlText(ttl time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case ttl >= 2*day && ttl%day == 0:
		return fmt.Sprintf("%d 天", ttl/day)
	case ttl >= time.Hour && ttl%time.Hour == 0:
		return fmt.Sprintf("%d 小时", ttl/time.Hour)
	case ttl >= time.Minute && ttl%time.Minute == 0:
		return fmt.Sprintf("%d 分钟", ttl/time.Minute)
	default:
		return ttl.String()
	}
}

// budgetProfileName names the Catalog budget profile a task's budget equals,
// "" when none does: a child's budget is its route's profile intersected with
// its parent's grant, which need not equal any profile.
func (s *Service) budgetProfileName(budget domain.Budget) string {
	for _, profile := range s.catalog.BudgetProfiles {
		if reflect.DeepEqual(profile.Budget(), budget) {
			return profile.Name
		}
	}
	return ""
}
