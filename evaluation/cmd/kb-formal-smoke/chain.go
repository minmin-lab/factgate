package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/kbformal"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

// Approval chains (KB1-LOOP-141; a demo feature, not a paper claim). When the
// Catalog routes requests through an org-chart chain, the smoke follows it:
// every step is decided by the approver TaskGate names, with the OA password
// OA_<SUBJECT>_PASSWORD or, for the demo's role accounts, the shared
// OA_SHARED_PASSWORD. A Catalog without chains keeps the single approver bob,
// as in the formal round.

// firstChain returns the Catalog's first approval chain, nil when it has none.
func firstChain(profile *catalog.Catalog) *catalog.ApprovalChain {
	for _, route := range profile.ApprovalRoutes {
		if route.Chain != nil {
			return route.Chain
		}
	}
	return nil
}

// chainApprovers resolves the OA account of each approver a chain names and
// records who decided, in order.
type chainApprovers struct {
	baseURL  string
	env      map[string]string
	timeout  time.Duration
	accounts map[string]*kbformal.OAAccount
	decided  []string
}

func newChainApprovers(baseURL string, env map[string]string, known *kbformal.OAAccount, timeout time.Duration) *chainApprovers {
	return &chainApprovers{baseURL: baseURL, env: env, timeout: timeout,
		accounts: map[string]*kbformal.OAAccount{known.Username: known}}
}

func (c *chainApprovers) account(subject string) (*kbformal.OAAccount, error) {
	if count := len(c.decided); count == 0 || c.decided[count-1] != subject {
		c.decided = append(c.decided, subject)
	}
	if account, ok := c.accounts[subject]; ok {
		return account, nil
	}
	password := oaPassword(c.env, subject)
	if password == "" {
		return nil, fmt.Errorf("no OA password for approver %s", subject)
	}
	account := &kbformal.OAAccount{BaseURL: c.baseURL, Username: subject, Password: password, Timeout: c.timeout}
	c.accounts[subject] = account
	return account, nil
}

// oaPassword is OA_<SUBJECT>_PASSWORD, or the demo's shared OA_SHARED_PASSWORD.
func oaPassword(env map[string]string, subject string) string {
	if own := env["OA_"+strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToUpper(subject))+"_PASSWORD"]; own != "" {
		return own
	}
	return env["OA_SHARED_PASSWORD"]
}

// smokeChain requests the requester's own department and another one,
// including the escalation value, so the chain has every kind of step: the
// requester's director, the other department's director, and the leaders. The
// approvers who decided must be the chain request_data_task announced, in
// order.
func smokeChain(ctx context.Context, summary map[string]any, provisioner *kbformal.Provisioner, approvers *chainApprovers,
	chain *catalog.ApprovalChain, products []string, columns map[string][]string, ownDepartment string) error {
	if chain.Escalation == nil || len(chain.Escalation.Values) == 0 {
		return errors.New("the chain has no escalation to exercise")
	}
	var others []string
	for department := range chain.Directors {
		if department != ownDepartment {
			others = append(others, department)
		}
	}
	sort.Strings(others)
	if len(others) == 0 {
		return errors.New("the chain's org chart has a single department")
	}
	request := kbformal.TaskRequest{Objective: "KB1-LOOP-141 smoke: cross-department request with leadership data",
		Products: products, Columns: columns,
		Scope: map[string]any{chain.DepartmentScope: []string{ownDepartment, others[0]},
			chain.Escalation.Scope: []string{"internal", chain.Escalation.Values[0]}}}
	approvers.decided = nil
	taskID, created, err := provisioner.Provision(ctx, request)
	if err != nil {
		return err
	}
	steps, _ := created["approval_chain"].([]any)
	var planned, roles []string
	for _, step := range steps {
		entry, _ := step.(map[string]any)
		approver, _ := entry["approver"].(string)
		role, _ := entry["role"].(string)
		planned = append(planned, approver)
		roles = append(roles, role)
	}
	if len(planned) < 3 || roles[0] != catalog.ApprovalStepRequesterDirector || roles[1] != catalog.ApprovalStepDepartmentDirector ||
		roles[len(roles)-1] != catalog.ApprovalStepLeader {
		return fmt.Errorf("announced chain %v (%v) lacks a requester director, a department director or a leader", planned, roles)
	}
	if strings.Join(planned, ",") != strings.Join(approvers.decided, ",") {
		return fmt.Errorf("approvers %v decided, but the announced chain is %v", approvers.decided, planned)
	}
	summary["approval_chain"] = planned
	summary["approval_chain_roles"] = roles
	if _, err := provisioner.Requester.Call(ctx, "revoke_task", map[string]any{"task_id": taskID, "reason": "kb-formal-smoke: approval chain probe"}); err != nil {
		return fmt.Errorf("revoke the chain probe: %w", err)
	}
	return nil
}
