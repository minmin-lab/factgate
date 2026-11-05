package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/kbformal"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

// Agent submission (KB1-LOOP-149; the Catalog's delegated_authorization
// agent_submits, a demo feature, not a paper claim). kbformal.Provisioner,
// frozen with the formal round, submits every draft in the OA before deciding
// it. Under agent_submits a task is created AWAITING_APPROVAL with its draft
// already pending, and the OA refuses a submission (409), so the smoke skips
// that step and has the task approved as made, following whoever
// get_task_status names (as kbformal.Provisioner.decide does). Without the
// switch the smoke provisions through kbformal.Provisioner unchanged.

// smokeProvisioner is kbformal.Provisioner, or the agent-submitted path when
// the profile Catalog switches agent_submits on.
type smokeProvisioner struct {
	*kbformal.Provisioner
	agentSubmits bool
}

// agentSubmitsOn reports whether the profile Catalog has the agent submit
// tasks for their human subject.
func agentSubmitsOn(profile *catalog.Catalog) bool {
	return profile.DelegatedAuthorization != nil && profile.DelegatedAuthorization.AgentSubmits
}

// Provision returns the task ID once the task is ACTIVE, and the
// request_data_task response.
func (p *smokeProvisioner) Provision(ctx context.Context, request kbformal.TaskRequest) (string, map[string]any, error) {
	if !p.agentSubmits {
		return p.Provisioner.Provision(ctx, request)
	}
	if request.TTL > 0 {
		return "", nil, errors.New("the smoke approves agent-submitted tasks as made; a narrowed TTL is not supported")
	}
	created, err := p.Requester.Call(ctx, "request_data_task", map[string]any{
		"objective": request.Objective, "data_products": request.Products, "columns": request.Columns, "scopes": request.Scope,
	})
	if err != nil {
		return "", nil, err
	}
	return p.approveSubmitted(ctx, created)
}

// ProvisionDownload is Provision for a document download task.
func (p *smokeProvisioner) ProvisionDownload(ctx context.Context, arguments map[string]any) (string, map[string]any, error) {
	if !p.agentSubmits {
		return p.Provisioner.ProvisionDownload(ctx, arguments)
	}
	created, err := p.Requester.Call(ctx, "request_download_task", arguments)
	if err != nil {
		return "", nil, err
	}
	return p.approveSubmitted(ctx, created)
}

// approveSubmitted has an agent-submitted task approved as made: each step
// by the approver get_task_status names (Approver when the smoke follows no
// chain), until the task is ACTIVE.
func (p *smokeProvisioner) approveSubmitted(ctx context.Context, created map[string]any) (string, map[string]any, error) {
	taskID, _ := created["task_id"].(string)
	oaURL, _ := created["oa_url"].(string)
	if taskID == "" || oaURL == "" {
		return "", created, errors.New("the task request omitted task_id or oa_url")
	}
	if created["state"] != "AWAITING_APPROVAL" || created["agent_submitted"] != true {
		return "", created, fmt.Errorf("task %s was created %v (agent_submitted %v); agent_submits wants AWAITING_APPROVAL",
			taskID, created["state"], created["agent_submitted"])
	}
	draftID := oaURL[strings.LastIndex(oaURL, "/")+1:]
	approved := url.Values{"decision": {"approved"}}
	deadline := time.Now().Add(p.Deadline)
	decided := map[string]bool{}
	var last error
	for time.Now().Before(deadline) {
		status, err := p.Requester.Call(ctx, "get_task_status", map[string]string{"task_id": taskID})
		if err != nil {
			last = err
			time.Sleep(p.Poll)
			continue
		}
		state, _ := status["state"].(string)
		if state == "ACTIVE" {
			return taskID, created, nil
		}
		if state != "AWAITING_APPROVAL" {
			return "", created, fmt.Errorf("task %s left its approval in state %q", taskID, state)
		}
		// TaskGate names an approver once, and never the same one twice; while
		// it still names one who decided, their decision is being applied.
		subject, _ := status["approver"].(string)
		if subject == "" || decided[subject] {
			time.Sleep(p.Poll)
			continue
		}
		account := p.Approver
		if p.ApproverFor != nil {
			if account, err = p.ApproverFor(subject); err != nil {
				return "", created, err
			}
		}
		// The OA reassigns the draft when TaskGate's answer reaches it, a moment
		// after get_task_status names the next approver; until then the next
		// approver is refused and retries.
		if last = account.Act(ctx, draftID, "decision", approved); last != nil {
			time.Sleep(p.Poll)
			continue
		}
		decided[subject] = true
	}
	return "", created, fmt.Errorf("agent-submitted task %s did not become ACTIVE (approvers %v, last error %v)", taskID, decided, last)
}
