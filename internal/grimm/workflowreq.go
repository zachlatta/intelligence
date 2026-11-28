package grimm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/workflow"
)

// MaxGrimmInputsPerRun bounds how many mid-run `ctl workflow input` messages Grimm
// may send to a single workflow run before the human posts a new message in the Grimm thread.
const MaxGrimmInputsPerRun = 5

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// resolveWorkflow finds an active (non-deleted) workflow for account by UUID or exact
// case-insensitive name, and checks optional hostFlag against its configured host.
func resolveWorkflow(ctx context.Context, tx pgx.Tx, account int64, ref, hostFlag string) (id, name, host string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", "", badRequest("workflow request needs --workflow <id|name>")
	}
	if ValidID(ref) {
		err = tx.QueryRow(ctx, `SELECT w.id::text, w.name, coalesce(v.definition->>'host', '')
			FROM workflows w
			JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
			WHERE w.account_id = $1 AND w.id = $2 AND w.deleted_at IS NULL`, account, ref).Scan(&id, &name, &host)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", "", refuse(404, "no active workflow with id %s", ref)
		}
		if err != nil {
			return "", "", "", err
		}
	} else {
		rows, qerr := tx.Query(ctx, `SELECT w.id::text, w.name, coalesce(v.definition->>'host', '')
			FROM workflows w
			JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
			WHERE w.account_id = $1 AND lower(w.name) = lower($2) AND w.deleted_at IS NULL
			ORDER BY w.updated_at DESC LIMIT 2`, account, ref)
		if qerr != nil {
			return "", "", "", qerr
		}
		type match struct{ id, name, host string }
		ms, cerr := pgx.CollectRows(rows, func(r pgx.CollectableRow) (match, error) {
			var m match
			return m, r.Scan(&m.id, &m.name, &m.host)
		})
		if cerr != nil {
			return "", "", "", cerr
		}
		switch len(ms) {
		case 0:
			return "", "", "", refuse(404, "no active workflow named %q", ref)
		case 1:
			id, name, host = ms[0].id, ms[0].name, ms[0].host
		default:
			return "", "", "", conflict("multiple workflows are named %q; pass the workflow UUID instead", ref)
		}
	}
	hostFlag = strings.TrimSpace(hostFlag)
	if hostFlag != "" && host != "" && hostFlag != host {
		return "", "", "", badRequest("workflow %q runs on host %s, not %s", name, host, hostFlag)
	}
	return id, name, host, nil
}

// resolveLinkedProposal finds a workflow proposal linked to sessionID by full UUID or prefix.
func resolveLinkedProposal(ctx context.Context, tx pgx.Tx, sessionID, ref string) (proposalID, wfName string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", badRequest("proposal id is required")
	}
	err = tx.QueryRow(ctx, `SELECT proposal_id::text, workflow_name
		FROM grimm_workflow_run_status
		WHERE session_id = $1 AND (proposal_id::text = $2 OR proposal_id::text LIKE $2 || '%')
		ORDER BY created_at DESC LIMIT 1`, sessionID, ref).Scan(&proposalID, &wfName)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", refuse(404, "no workflow proposal matching %q linked to this session", ref)
	}
	return proposalID, wfName, err
}

// resolveLinkedRun finds a started workflow run linked to sessionID by run UUID/prefix or proposal UUID/prefix.
func resolveLinkedRun(ctx context.Context, tx pgx.Tx, sessionID, ref string) (runID, proposalID, wfName, host string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", "", "", badRequest("workflow run id is required")
	}
	err = tx.QueryRow(ctx, `SELECT coalesce(run_id::text, ''), proposal_id::text, workflow_name, host
		FROM grimm_workflow_run_status
		WHERE session_id = $1 AND (
			run_id::text = $2 OR run_id::text LIKE $2 || '%'
			OR proposal_id::text = $2 OR proposal_id::text LIKE $2 || '%'
		)
		ORDER BY created_at DESC LIMIT 1`, sessionID, ref).Scan(&runID, &proposalID, &wfName, &host)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", "", refuse(404, "no workflow run matching %q linked to this session", ref)
	}
	if err != nil {
		return "", "", "", "", err
	}
	if runID == "" {
		return "", "", "", "", conflict("workflow proposal %s has not started a run yet", shortID(proposalID))
	}
	return runID, proposalID, wfName, host, nil
}

func defaultWorkflowSummary(wfName, input string) string {
	first := strings.TrimSpace(strings.Split(strings.TrimSpace(input), "\n")[0])
	if first == "" {
		return "Run " + wfName
	}
	return cut("Run "+wfName+": "+first, 180)
}

// requestWorkflow creates a pending workflow run proposal for the session (never starting a run),
// records the grimm_workflow_runs link and thread card item, and notifies the session stream.
func (s *Service) requestWorkflow(ctx context.Context, account int64, host, agent string, c Call) (string, error) {
	if s.Workflows == nil {
		return "", refuse(503, "workflow engine is not configured")
	}
	input := strings.TrimSpace(c.Text)
	if input == "" {
		return "", badRequest("workflow request needs task input")
	}
	if repo := strings.TrimSpace(c.Repo); repo != "" {
		if !strings.HasPrefix(strings.ToLower(input), "repo:") {
			input = "Repo: " + repo + "\n\n" + input
		}
	}
	if len(input) > workflow.MaxAPITextBytes {
		return "", badRequest("workflow request input is longer than 1 MiB")
	}
	var who Caller
	var sess Session
	var wfID, wfName, wfHost, replacesID string
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		who, err = whoIs(ctx, tx, account, host, agent)
		if err != nil {
			return err
		}
		if who.Role != "manager" {
			return refuse(403, "only the Grimm manager can request workflow runs; post to grimm on the bus instead")
		}
		sess, err = activeSession(ctx, tx, 0, who.Session)
		if err != nil {
			return err
		}
		wfID, wfName, wfHost, err = resolveWorkflow(ctx, tx, sess.Account, c.Workflow, c.Host)
		if err != nil {
			return err
		}
		if strings.TrimSpace(c.Replaces) != "" {
			replacesID, _, err = resolveLinkedProposal(ctx, tx, sess.ID, c.Replaces)
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return "", err
	}

	summary := strings.TrimSpace(c.Summary)
	if summary == "" {
		summary = defaultWorkflowSummary(wfName, input)
	}
	propID, err := s.Workflows.Propose(ctx, sess.Account, "host:"+sess.Host, workflow.ProposalRequest{
		Operation:  "run",
		WorkflowID: wfID,
		Input:      input,
		Summary:    summary,
		ReplacesID: replacesID,
		ProposedBy: "grimm:" + sess.ID + ":" + agent,
	})
	if err != nil {
		return "", mapWorkflowError(err)
	}

	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		locked, err := activeSession(ctx, tx, 0, sess.ID)
		if err != nil {
			return err
		}
		cardData := map[string]any{
			"proposalId":   propID,
			"workflowId":   wfID,
			"workflowName": wfName,
			"host":         wfHost,
		}
		if repo := strings.TrimSpace(c.Repo); repo != "" {
			cardData["repo"] = repo
		}
		if replacesID != "" {
			cardData["replacesId"] = replacesID
			if _, err := tx.Exec(ctx, `UPDATE grimm_workflow_runs SET last_proposal_status = 'withdrawn' WHERE proposal_id = $1 AND session_id = $2`, replacesID, locked.ID); err != nil {
				return err
			}
		}
		msgID, err := threadRow(ctx, tx, locked, "workflow_request", ManagerName, summary, cardData)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO grimm_workflow_runs (proposal_id, session_id, account_id, requested_by, message_id)
			VALUES ($1, $2, $3, $4, $5)`, propID, locked.ID, locked.Account, agent, msgID); err != nil {
			return err
		}
		if err := event(ctx, tx, locked, ManagerName, "workflow_requested", "info",
			fmt.Sprintf("Requested workflow %s (pending approval)", wfName),
			map[string]string{"proposalId": propID, "workflowId": wfID}); err != nil {
			return err
		}
		return notify(ctx, tx, locked.ID)
	}); err != nil {
		return "", err
	}
	if replacesID != "" {
		return fmt.Sprintf("requested workflow %s (%s): proposal %s is pending approval in the web UI (replaced %s; no run started yet)", wfName, wfHost, propID, shortID(replacesID)), nil
	}
	return fmt.Sprintf("requested workflow %s (%s): proposal %s is pending approval in the web UI (no run started yet)", wfName, wfHost, propID), nil
}

// withdrawWorkflow withdraws a pending workflow proposal created by this session.
func (s *Service) withdrawWorkflow(ctx context.Context, account int64, host, agent string, c Call) (string, error) {
	if s.Workflows == nil {
		return "", refuse(503, "workflow engine is not configured")
	}
	if len(c.Args) < 2 {
		return "", badRequest("workflow withdraw <proposal-id> --reason <why>")
	}
	reason := strings.TrimSpace(c.Reason)
	if reason == "" {
		return "", badRequest("workflow withdraw needs --reason <why>")
	}
	var sess Session
	var propID, wfName string
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		who, err := whoIs(ctx, tx, account, host, agent)
		if err != nil {
			return err
		}
		if who.Role != "manager" {
			return refuse(403, "only the Grimm manager can withdraw workflow proposals; post to grimm on the bus instead")
		}
		sess, err = activeSession(ctx, tx, 0, who.Session)
		if err != nil {
			return err
		}
		propID, wfName, err = resolveLinkedProposal(ctx, tx, sess.ID, c.Args[1])
		return err
	}); err != nil {
		return "", err
	}
	if err := s.Workflows.Withdraw(ctx, sess.Account, "host:"+sess.Host, propID, reason); err != nil {
		return "", mapWorkflowError(err)
	}
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		locked, err := activeSession(ctx, tx, 0, sess.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE grimm_workflow_runs SET last_proposal_status = 'withdrawn' WHERE proposal_id = $1 AND session_id = $2`, propID, locked.ID); err != nil {
			return err
		}
		if err := event(ctx, tx, locked, ManagerName, "workflow_withdrawn", "info",
			fmt.Sprintf("Withdrew workflow proposal %s (%s): %s", shortID(propID), wfName, cut(reason, 200)),
			map[string]string{"proposalId": propID}); err != nil {
			return err
		}
		return notify(ctx, tx, locked.ID)
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("withdrew proposal %s (%s): %s", shortID(propID), wfName, reason), nil
}

// inputWorkflow sends mid-run guidance from the Grimm manager to a linked running or stalled run,
// subject to a per-run rate limit that resets whenever the human posts in the Grimm thread.
func (s *Service) inputWorkflow(ctx context.Context, account int64, host, agent string, c Call) (string, error) {
	if s.Workflows == nil {
		return "", refuse(503, "workflow engine is not configured")
	}
	if len(c.Args) < 2 {
		return "", badRequest("workflow input <run> <text>|-")
	}
	text := strings.TrimSpace(c.Text)
	if text == "" {
		return "", badRequest("workflow input needs text")
	}
	if len(text) > workflow.MaxAPITextBytes {
		return "", badRequest("workflow input is longer than 1 MiB")
	}
	var sess Session
	var runID, propID, wfName string
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		who, err := whoIs(ctx, tx, account, host, agent)
		if err != nil {
			return err
		}
		if who.Role != "manager" {
			return refuse(403, "only the Grimm manager can send input to workflow runs; post to grimm on the bus instead")
		}
		sess, err = activeSession(ctx, tx, 0, who.Session)
		if err != nil {
			return err
		}
		runID, propID, wfName, _, err = resolveLinkedRun(ctx, tx, sess.ID, c.Args[1])
		if err != nil {
			return err
		}
		var sent int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_run_events
			WHERE run_id = $1 AND grimm_session_id = $2 AND kind = 'input'
			  AND at > coalesce((SELECT max(created_at) FROM grimm_messages WHERE session_id = $2 AND kind = 'human'), 'epoch'::timestamptz)`,
			runID, sess.ID).Scan(&sent); err != nil {
			return err
		}
		if sent >= MaxGrimmInputsPerRun {
			return refuse(429, "rate limit reached: already sent %d inputs to run %s since the human's last message; wait for the human or run progress", sent, shortID(runID))
		}
		return nil
	}); err != nil {
		return "", err
	}
	actCtx := workflow.WithGrimmActor(ctx, workflow.GrimmActor{SessionID: sess.ID, AgentID: agent})
	if err := s.Workflows.AddInput(actCtx, sess.Account, runID, text); err != nil {
		return "", mapWorkflowError(err)
	}
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		locked, err := activeSession(ctx, tx, 0, sess.ID)
		if err != nil {
			return err
		}
		if err := event(ctx, tx, locked, ManagerName, "workflow_input", "info",
			fmt.Sprintf("Sent guidance to %s (%s): %s", wfName, shortID(runID), cut(oneLine(text), 200)),
			map[string]string{"proposalId": propID, "runId": runID}); err != nil {
			return err
		}
		return notify(ctx, tx, locked.ID)
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("sent input to %s (run %s)", wfName, shortID(runID)), nil
}

// replyWorkflow answers a waiting clarification ask on a linked run.
// The workflow engine hard-refuses sign-off steps and secret asks with 403.
func (s *Service) replyWorkflow(ctx context.Context, account int64, host, agent string, c Call) (string, error) {
	if s.Workflows == nil {
		return "", refuse(503, "workflow engine is not configured")
	}
	if len(c.Args) < 2 {
		return "", badRequest("workflow reply <run> <text>|-")
	}
	text := strings.TrimSpace(c.Text)
	if text == "" {
		return "", badRequest("workflow reply needs text")
	}
	if len(text) > workflow.MaxAPITextBytes {
		return "", badRequest("workflow reply is longer than 1 MiB")
	}
	var sess Session
	var runID, propID, wfName string
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		who, err := whoIs(ctx, tx, account, host, agent)
		if err != nil {
			return err
		}
		if who.Role != "manager" {
			return refuse(403, "only the Grimm manager can reply to workflow asks; post to grimm on the bus instead")
		}
		sess, err = activeSession(ctx, tx, 0, who.Session)
		if err != nil {
			return err
		}
		runID, propID, wfName, _, err = resolveLinkedRun(ctx, tx, sess.ID, c.Args[1])
		return err
	}); err != nil {
		return "", err
	}
	actCtx := workflow.WithGrimmActor(ctx, workflow.GrimmActor{SessionID: sess.ID, AgentID: agent})
	if err := s.Workflows.Reply(actCtx, sess.Account, runID, text); err != nil {
		return "", mapWorkflowError(err)
	}
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		locked, err := activeSession(ctx, tx, 0, sess.ID)
		if err != nil {
			return err
		}
		if err := event(ctx, tx, locked, ManagerName, "workflow_reply", "info",
			fmt.Sprintf("Replied to %s (%s): %s", wfName, shortID(runID), cut(oneLine(text), 200)),
			map[string]string{"proposalId": propID, "runId": runID}); err != nil {
			return err
		}
		return notify(ctx, tx, locked.ID)
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("replied to %s (run %s)", wfName, shortID(runID)), nil
}

// cancelWorkflow cancels a linked workflow run with a required reason (only when the human instructed cancellation).
func (s *Service) cancelWorkflow(ctx context.Context, account int64, host, agent string, c Call) (string, error) {
	if s.Workflows == nil {
		return "", refuse(503, "workflow engine is not configured")
	}
	if len(c.Args) < 2 {
		return "", badRequest("workflow cancel <run> --why <reason>")
	}
	reason := strings.TrimSpace(c.Reason)
	if reason == "" {
		return "", badRequest("workflow cancel needs --why <reason>")
	}
	var sess Session
	var runID, propID, wfName string
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		who, err := whoIs(ctx, tx, account, host, agent)
		if err != nil {
			return err
		}
		if who.Role != "manager" {
			return refuse(403, "only the Grimm manager can cancel workflow runs; post to grimm on the bus instead")
		}
		sess, err = activeSession(ctx, tx, 0, who.Session)
		if err != nil {
			return err
		}
		runID, propID, wfName, _, err = resolveLinkedRun(ctx, tx, sess.ID, c.Args[1])
		return err
	}); err != nil {
		return "", err
	}
	actCtx := workflow.WithGrimmActor(ctx, workflow.GrimmActor{SessionID: sess.ID, AgentID: agent})
	if err := s.Workflows.CancelWithReason(actCtx, sess.Account, runID, reason); err != nil {
		return "", mapWorkflowError(err)
	}
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		locked, err := activeSession(ctx, tx, 0, sess.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE grimm_workflow_runs SET last_run_status = 'canceled' WHERE proposal_id = $1 AND session_id = $2`, propID, locked.ID); err != nil {
			return err
		}
		if err := event(ctx, tx, locked, ManagerName, "workflow_canceled", "info",
			fmt.Sprintf("Canceled %s (%s): %s", wfName, shortID(runID), cut(reason, 200)),
			map[string]string{"proposalId": propID, "runId": runID}); err != nil {
			return err
		}
		return notify(ctx, tx, locked.ID)
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("canceled %s (run %s): %s", wfName, shortID(runID), reason), nil
}

func mapWorkflowError(err error) error {
	var wr *workflow.Refusal
	if errors.As(err, &wr) {
		status := 400
		if wr.Forbidden {
			status = 403
		} else if wr.Conflict {
			status = 409
		}
		return &Error{Status: status, Msg: wr.Msg}
	}
	if errors.Is(err, workflow.ErrNotFound) {
		return refuse(404, "workflow not found")
	}
	return err
}

// UpdateWorkflowRequestInput lets the signed-in human edit a pending, unexpired Grimm workflow
// run proposal's input before approving it.
func (s *Service) UpdateWorkflowRequestInput(ctx context.Context, account int64, sessionID, proposalID, input string) error {
	if !ValidID(sessionID) || !ValidID(proposalID) {
		return ErrNotFound
	}
	input = strings.TrimSpace(input)
	if input == "" || len(input) > workflow.MaxAPITextBytes {
		return badRequest("run input must contain 1 to 1048576 bytes")
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		sess, err := activeSession(ctx, tx, account, sessionID)
		if err != nil {
			return err
		}
		var status, op string
		var expired bool
		err = tx.QueryRow(ctx, `SELECT p.status, p.operation, p.expires_at <= $4
			FROM grimm_workflow_runs g
			JOIN workflow_proposals p ON p.id = g.proposal_id
			WHERE g.session_id = $1 AND g.proposal_id = $2 AND p.account_id = $3
			FOR UPDATE OF p`, sess.ID, proposalID, account, s.now()).Scan(&status, &op, &expired)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse(404, "no such workflow request in this session")
		}
		if err != nil {
			return err
		}
		if op != "run" {
			return badRequest("only workflow run requests can be edited")
		}
		if status != "pending" || expired {
			return conflict("cannot edit input: request is no longer pending")
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_proposals SET input = $2 WHERE id = $1`, proposalID, input); err != nil {
			return err
		}
		if err := event(ctx, tx, sess, "human", "workflow_input_edited", "info",
			"Edited task input for pending workflow request "+shortID(proposalID),
			map[string]string{"proposalId": proposalID}); err != nil {
			return err
		}
		return notify(ctx, tx, sess.ID)
	})
}

type wfStepSummary struct {
	Index   int    `json:"index"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	Outcome string `json:"outcome"`
}

// workflowCatalogText lists all active saved workflows for account (name, trigger, host, steps, description).
func workflowCatalogText(ctx context.Context, tx pgx.Tx, account int64) (string, error) {
	rows, err := tx.Query(ctx, `SELECT w.id::text, w.name, w.description,
		coalesce(v.definition->>'trigger', ''), coalesce(v.definition->>'host', ''),
		coalesce(v.definition->'steps', '[]'::jsonb)
		FROM workflows w
		JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
		WHERE w.account_id = $1 AND w.deleted_at IS NULL
		ORDER BY w.is_default DESC, lower(w.name)`, account)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	count := 0
	for rows.Next() {
		var id, name, desc, trigger, host string
		var stepsRaw []byte
		if err := rows.Scan(&id, &name, &desc, &trigger, &host, &stepsRaw); err != nil {
			return "", err
		}
		count++
		var steps []struct {
			Title string `json:"title"`
			Kind  string `json:"kind"`
		}
		_ = json.Unmarshal(stepsRaw, &steps)
		var stepNames []string
		for _, st := range steps {
			label := st.Title
			if st.Kind == workflow.KindGate || st.Kind == workflow.KindSignoff {
				label += " [" + st.Kind + "]"
			}
			stepNames = append(stepNames, label)
		}
		fmt.Fprintf(&b, "%s  %s  trigger=%s host=%s\n", shortID(id), name, trigger, host)
		if len(stepNames) > 0 {
			fmt.Fprintf(&b, "  steps: %s\n", strings.Join(stepNames, " -> "))
		}
		if d := strings.TrimSpace(desc); d != "" {
			fmt.Fprintf(&b, "  desc:  %s\n", cut(oneLine(d), 200))
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 0 {
		return "(no saved workflows)", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func workflowListText(ctx context.Context, tx pgx.Tx, sessionID string) (string, error) {
	rows, err := tx.Query(ctx, `SELECT proposal_id::text, workflow_name, host, proposal_status,
		coalesce(run_id::text, ''), run_status, run_step, step_count, step_title
		FROM grimm_workflow_run_status WHERE session_id = $1 ORDER BY created_at`, sessionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var pid, wfName, host, pStatus, runID, rStatus, stepTitle string
		var step, stepCount int
		if err := rows.Scan(&pid, &wfName, &host, &pStatus, &runID, &rStatus, &step, &stepCount, &stepTitle); err != nil {
			return "", err
		}
		line := fmt.Sprintf("%s  %s (%s)  proposal=%s", shortID(pid), wfName, host, pStatus)
		if runID != "" {
			line += fmt.Sprintf("  run=%s status=%s", shortID(runID), rStatus)
			if step >= 0 && stepCount > 0 {
				line += fmt.Sprintf(" step=%d/%d", step+1, stepCount)
				if stepTitle != "" {
					line += " (" + stepTitle + ")"
				}
			}
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "(no workflow requests in this session)", nil
	}
	return strings.Join(lines, "\n"), nil
}

func workflowStatusText(ctx context.Context, tx pgx.Tx, sessionID, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", badRequest("workflow status needs a proposal or run id")
	}
	var pid, wfName, host, summary, input, pStatus, runID, rStatus, stepTitle, stepKind, note, rErr, waitingOn, queueReason, askKind, doneSummary string
	var step, stepCount int
	var stepsRaw, delivRaw []byte
	err := tx.QueryRow(ctx, `SELECT proposal_id::text, workflow_name, host, summary, input, proposal_status,
		coalesce(run_id::text, ''), run_status, run_step, step_count, step_title, step_kind,
		run_note, run_error, waiting_on, steps, queue_reason, ask_kind, done_summary, deliverables
		FROM grimm_workflow_run_status
		WHERE session_id = $1 AND (
			proposal_id::text = $2 OR proposal_id::text LIKE $2 || '%'
			OR run_id::text = $2 OR run_id::text LIKE $2 || '%'
		)
		ORDER BY created_at DESC LIMIT 1`, sessionID, target).Scan(
		&pid, &wfName, &host, &summary, &input, &pStatus,
		&runID, &rStatus, &step, &stepCount, &stepTitle, &stepKind,
		&note, &rErr, &waitingOn, &stepsRaw, &queueReason, &askKind, &doneSummary, &delivRaw,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", refuse(404, "no workflow request or run matching %q in this session", target)
	}
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Workflow: %s (%s)\n", wfName, host)
	fmt.Fprintf(&b, "Proposal: %s (%s)\n", pid, pStatus)
	if summary != "" {
		fmt.Fprintf(&b, "Summary:  %s\n", summary)
	}
	if runID != "" {
		fmt.Fprintf(&b, "Run:      %s (%s)\n", runID, rStatus)
		if queueReason != "" && rStatus == "queued" {
			fmt.Fprintf(&b, "Queue:    %s\n", queueReason)
		}
		if step >= 0 && stepCount > 0 {
			fmt.Fprintf(&b, "Step:     %d/%d %s", step+1, stepCount, stepTitle)
			if stepKind != "" && stepKind != "step" {
				fmt.Fprintf(&b, " [%s]", stepKind)
			}
			b.WriteByte('\n')
		}
		var steps []wfStepSummary
		if json.Unmarshal(stepsRaw, &steps) == nil && len(steps) > 0 {
			var gates []string
			for _, st := range steps {
				if st.Kind == workflow.KindGate || st.Kind == workflow.KindSignoff {
					gates = append(gates, fmt.Sprintf("%s=%s", st.Title, st.Outcome))
				}
			}
			if len(gates) > 0 {
				fmt.Fprintf(&b, "Gates:    %s\n", strings.Join(gates, ", "))
			}
		}
		if waitingOn != "" {
			fmt.Fprintf(&b, "Waiting:  %s\n", waitingOn)
		}
		if askKind != "" && rStatus == "waiting" {
			fmt.Fprintf(&b, "AskKind:  %s\n", askKind)
		}
		if note != "" && rStatus == "waiting" {
			fmt.Fprintf(&b, "Question: %s\n", note)
		}
		if doneSummary != "" && rStatus == "succeeded" {
			fmt.Fprintf(&b, "Outcome:  %s\n", doneSummary)
		}
		var deliverables []string
		if json.Unmarshal(delivRaw, &deliverables) == nil && len(deliverables) > 0 {
			fmt.Fprintf(&b, "Claims:   %s\n", strings.Join(deliverables, ", "))
		}
		if rErr != "" {
			fmt.Fprintf(&b, "Error:    %s\n", cut(rErr, 500))
		}
	}
	fmt.Fprintf(&b, "Input:\n%s", cut(input, 1000))
	return b.String(), nil
}

// workflowEventsText reads workflow_run_events chronologically for a linked run.
func workflowEventsText(ctx context.Context, tx pgx.Tx, sessionID, ref string, since int64, limit int) (string, error) {
	runID, _, _, _, err := resolveLinkedRun(ctx, tx, sessionID, ref)
	if err != nil {
		return "", err
	}
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	rows, err := tx.Query(ctx, `SELECT id, at, kind, step, to_step, text, coalesce(grimm_session_id::text, '')
		FROM workflow_run_events
		WHERE run_id = $1 AND id > $2
		ORDER BY id ASC LIMIT $3`, runID, since, limit)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id int64
		var at time.Time
		var kind, text, grimmSess string
		var step, toStep *int
		if err := rows.Scan(&id, &at, &kind, &step, &toStep, &text, &grimmSess); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "#%d %s %s", id, at.UTC().Format(time.RFC3339), kind)
		if step != nil {
			fmt.Fprintf(&b, " step=%d", *step+1)
		}
		if toStep != nil {
			fmt.Fprintf(&b, "->%d", *toStep+1)
		}
		if grimmSess != "" {
			fmt.Fprintf(&b, " [grimm:%s]", shortID(grimmSess))
		}
		if t := strings.TrimSpace(text); t != "" {
			fmt.Fprintf(&b, ": %s", cut(oneLine(t), 400))
		}
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if b.Len() == 0 {
		return "(no matching events)", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// workflowPeekText tails recent transcript entries of a linked run's orchestrator or worker agent.
func workflowPeekText(ctx context.Context, tx pgx.Tx, sessionID, ref, agentFlag string, lines int) (string, error) {
	runID, _, _, _, err := resolveLinkedRun(ctx, tx, sessionID, ref)
	if err != nil {
		return "", err
	}
	var host, orchAgent, projAgent string
	if err := tx.QueryRow(ctx, `SELECT host, orchestrator_id, project_agent_id FROM workflow_runs WHERE id = $1`, runID).
		Scan(&host, &orchAgent, &projAgent); err != nil {
		return "", err
	}
	target := strings.TrimSpace(agentFlag)
	if target == "" || target == "orchestrator" {
		target = orchAgent
		if target == "" {
			return "(orchestrator has not started yet)", nil
		}
	} else if target != orchAgent && target != projAgent {
		var belongs bool
		if err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM agents WHERE host = $1 AND id = $2 AND labels->>'intelligence.run' = $3)
			OR EXISTS (SELECT 1 FROM workflow_run_agents WHERE run_id = $3 AND agent_id = $2)`,
			host, target, runID).Scan(&belongs); err != nil {
			return "", err
		}
		if !belongs {
			return "", refuse(404, "agent %q does not belong to workflow run %s", target, shortID(runID))
		}
	}
	if lines <= 0 {
		lines = 40
	}
	lines = max(1, min(lines, 400))
	rows, err := tx.Query(ctx, `SELECT * FROM (SELECT seq_start, kind, tool, status, text, item FROM transcript_entries
		WHERE host = $1 AND agent_id = $2 ORDER BY seq_start DESC LIMIT $3) x ORDER BY seq_start`, host, target, lines)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var seq int
		var kind, tool, status, text string
		var item json.RawMessage
		if err := rows.Scan(&seq, &kind, &tool, &status, &text, &item); err != nil {
			return "", err
		}
		b.WriteString(EntryLine(kind, tool, status, text, item))
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if b.Len() == 0 {
		return "(no transcript activity yet)", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// reconcileWorkflowRuns checks every workflow request in sess for proposal decisions and run
// milestones, updating grimm_workflow_runs and queueing debounced "[Grimm update]" wakeups.
func (s *Service) reconcileWorkflowRuns(ctx context.Context, tx pgx.Tx, sess Session) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT g.proposal_id::text, g.last_proposal_status, g.last_run_status, g.last_run_step, g.last_run_note,
		g.last_queue_reason, g.last_back_event_id,
		s.workflow_name, s.proposal_status, coalesce(s.run_id::text, ''), s.run_status, s.run_step, s.step_count, s.step_title, s.step_kind,
		s.run_note, s.run_error, s.waiting_on,
		s.queue_reason, s.ask_kind, s.last_back_event_id, s.last_back_reason, s.last_back_step, s.last_back_to_step,
		s.done_summary, s.deliverables
		FROM grimm_workflow_runs g
		JOIN grimm_workflow_run_status s ON s.proposal_id = g.proposal_id
		WHERE g.session_id = $1
		ORDER BY g.created_at`, sess.ID)
	if err != nil {
		return false, err
	}
	type row struct {
		propID, lastProp, lastRun, lastNote string
		lastStep                            int
		lastQueueReason                     string
		lastBackEventID                     int64
		wfName, propStatus, runID           string
		runStatus                           string
		runStep, stepCount                  int
		stepTitle, stepKind                 string
		runNote, runError, waitingOn        string
		queueReason, askKind                string
		backEventID                         int64
		backReason                          string
		backStep, backToStep                int
		doneSummary                         string
		deliverablesRaw                     []byte
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.propID, &x.lastProp, &x.lastRun, &x.lastStep, &x.lastNote,
			&x.lastQueueReason, &x.lastBackEventID,
			&x.wfName, &x.propStatus, &x.runID, &x.runStatus, &x.runStep, &x.stepCount, &x.stepTitle, &x.stepKind,
			&x.runNote, &x.runError, &x.waitingOn,
			&x.queueReason, &x.askKind, &x.backEventID, &x.backReason, &x.backStep, &x.backToStep,
			&x.doneSummary, &x.deliverablesRaw)
	})
	if err != nil {
		return false, err
	}
	changed := false
	for _, x := range list {
		propChanged := x.propStatus != x.lastProp
		backChanged := x.runID != "" && x.backEventID > x.lastBackEventID
		queueReasonChanged := x.runID != "" && x.runStatus == "queued" && x.queueReason != x.lastQueueReason
		dequeued := x.runID != "" && x.lastRun == "queued" && x.runStatus != "queued" && x.runStatus != ""
		runChanged := x.runID != "" && (x.runStatus != x.lastRun || x.runStep != x.lastStep || (x.runStatus == "waiting" && x.runNote != x.lastNote) || backChanged || queueReasonChanged)
		if !propChanged && !runChanged {
			continue
		}
		changed = true
		if _, err := tx.Exec(ctx, `UPDATE grimm_workflow_runs
			SET last_proposal_status = $2, last_run_status = $3, last_run_step = $4, last_run_note = $5,
			    last_queue_reason = $6, last_back_event_id = $7
			WHERE proposal_id = $1`, x.propID, x.propStatus, x.runStatus, x.runStep, x.runNote, x.queueReason, x.backEventID); err != nil {
			return false, err
		}

		// Proposal decision wakeup (when rejected/expired/stale/withdrawn, or approved without run update below).
		if propChanged && x.propStatus != "pending" && x.runID == "" {
			sum := fmt.Sprintf("workflow %s %s", x.wfName, x.propStatus)
			detail := fmt.Sprintf("Workflow request **%s** (proposal `%s`) was **%s**.", x.wfName, shortID(x.propID), x.propStatus)
			if err := s.wake(ctx, tx, sess, "", sum, detail); err != nil {
				return false, err
			}
			if err := event(ctx, tx, sess, "intelligence", "workflow_"+x.propStatus, "info", sum, map[string]string{"proposalId": x.propID}); err != nil {
				return false, err
			}
			continue
		}

		if x.runID == "" {
			continue
		}
		shortRun := shortID(x.runID)
		stepLabel := ""
		if x.runStep >= 0 && x.stepCount > 0 {
			stepLabel = fmt.Sprintf("step %d/%d", x.runStep+1, x.stepCount)
			if x.stepTitle != "" {
				stepLabel += " (" + x.stepTitle + ")"
			}
		}

		var sum, detail, evKind, evLevel string
		evLevel = "info"
		switch {
		case propChanged && x.propStatus == "applied":
			evKind = "workflow_started"
			sum = fmt.Sprintf("workflow %s approved & started (%s)", x.wfName, x.runStatus)
			detail = fmt.Sprintf("Workflow **%s** was approved and started (run `%s`, status: `%s`", x.wfName, shortRun, x.runStatus)
			if stepLabel != "" {
				detail += ", " + stepLabel
			}
			if x.runStatus == "queued" && x.queueReason != "" {
				detail += ", queue reason: " + x.queueReason
			}
			detail += ")."
		case x.runStatus == "queued":
			evKind = "workflow_queued"
			sum = fmt.Sprintf("workflow %s queued", x.wfName)
			detail = fmt.Sprintf("Workflow **%s** (run `%s`) is **queued**", x.wfName, shortRun)
			if x.queueReason != "" {
				detail += ": " + x.queueReason
			} else {
				detail += "."
			}
		case dequeued && x.runStatus != "waiting" && x.runStatus != "succeeded" && x.runStatus != "failed" && x.runStatus != "canceled" && x.runStatus != "stalled":
			evKind = "workflow_dequeued"
			sum = fmt.Sprintf("workflow %s dequeued (%s)", x.wfName, x.runStatus)
			detail = fmt.Sprintf("Workflow **%s** (run `%s`) **dequeued** and is now `%s`", x.wfName, shortRun, x.runStatus)
			if stepLabel != "" {
				detail += " at " + stepLabel
			}
			detail += "."
		case backChanged && x.runStatus != "waiting" && x.runStatus != "failed" && x.runStatus != "canceled":
			evKind = "workflow_back"
			evLevel = "warn"
			sum = fmt.Sprintf("workflow %s sent back to %s", x.wfName, stepLabel)
			detail = fmt.Sprintf("Workflow **%s** (run `%s`) was **sent back** to **%s**", x.wfName, shortRun, stepLabel)
			if x.backReason != "" {
				detail += ": " + x.backReason
			} else {
				detail += "."
			}
		case x.runStatus == "waiting":
			evKind = "workflow_waiting"
			evLevel = "warn"
			askKind := x.askKind
			if askKind == "" {
				askKind = "clarification"
			}
			switch askKind {
			case "signoff":
				sum = fmt.Sprintf("workflow %s waiting for human sign-off", x.wfName)
				detail = fmt.Sprintf("Workflow **%s** (run `%s`) is **waiting for human sign-off** (`ask_kind=signoff`)", x.wfName, shortRun)
			case "secret":
				sum = fmt.Sprintf("workflow %s waiting for secret", x.wfName)
				detail = fmt.Sprintf("Workflow **%s** (run `%s`) is **waiting for a secret** (`ask_kind=secret`)", x.wfName, shortRun)
			default:
				sum = fmt.Sprintf("workflow %s waiting for clarification", x.wfName)
				detail = fmt.Sprintf("Workflow **%s** (run `%s`) is **waiting for clarification** (`ask_kind=clarification`)", x.wfName, shortRun)
			}
			if stepLabel != "" {
				detail += " at " + stepLabel
			}
			if x.runNote != "" {
				detail += ":\n\n> " + strings.ReplaceAll(x.runNote, "\n", "\n> ")
			} else if x.waitingOn != "" {
				detail += " (" + x.waitingOn + ")."
			} else {
				detail += "."
			}
			switch askKind {
			case "signoff":
				detail += "\n\n(The human must review and sign off in the browser; Grimm cannot approve sign-off.)"
			case "secret":
				detail += "\n\n(The human must provide the secret in the browser; Grimm cannot supply secrets.)"
			default:
				detail += fmt.Sprintf("\n\n(If you already have the answer or context from the human, reply with `%s workflow reply %s - <<'EOF'`; otherwise ask the human in this chat.)", ctlCmd, shortRun)
			}
		case x.runStatus == "succeeded":
			evKind = "workflow_succeeded"
			sum = fmt.Sprintf("workflow %s succeeded", x.wfName)
			detail = fmt.Sprintf("Workflow **%s** (run `%s`) **succeeded**.", x.wfName, shortRun)
			if s := strings.TrimSpace(x.doneSummary); s != "" {
				detail += "\n\nSummary: " + s
			}
			var deliverables []string
			if json.Unmarshal(x.deliverablesRaw, &deliverables) == nil && len(deliverables) > 0 {
				detail += "\n\nDeliverables: " + strings.Join(deliverables, ", ")
			}
		case x.runStatus == "failed":
			evKind = "workflow_failed"
			evLevel = "warn"
			sum = fmt.Sprintf("workflow %s failed", x.wfName)
			detail = fmt.Sprintf("Workflow **%s** (run `%s`) **failed**", x.wfName, shortRun)
			if x.runError != "" {
				detail += ": " + cut(x.runError, 600)
			} else {
				detail += "."
			}
		case x.runStatus == "canceled":
			evKind = "workflow_canceled"
			sum = fmt.Sprintf("workflow %s canceled", x.wfName)
			detail = fmt.Sprintf("Workflow **%s** (run `%s`) was **canceled**.", x.wfName, shortRun)
		case x.runStatus == "stalled":
			evKind = "workflow_stalled"
			evLevel = "warn"
			sum = fmt.Sprintf("workflow %s stalled", x.wfName)
			detail = fmt.Sprintf("Workflow **%s** (run `%s`) **stalled**", x.wfName, shortRun)
			if stepLabel != "" {
				detail += " at " + stepLabel
			}
			if x.runError != "" {
				detail += ": " + cut(x.runError, 400)
			} else {
				detail += "."
			}
		case x.runStep != x.lastStep && x.runStep >= 0:
			evKind = "workflow_step"
			sum = fmt.Sprintf("workflow %s -> %s", x.wfName, stepLabel)
			detail = fmt.Sprintf("Workflow **%s** (run `%s`) moved to **%s** (status: `%s`).", x.wfName, shortRun, stepLabel, x.runStatus)
		case x.runStatus != x.lastRun:
			evKind = "workflow_status"
			sum = fmt.Sprintf("workflow %s is %s", x.wfName, x.runStatus)
			detail = fmt.Sprintf("Workflow **%s** (run `%s`) is now `%s`", x.wfName, shortRun, x.runStatus)
			if stepLabel != "" {
				detail += " at " + stepLabel
			}
			detail += "."
		}
		if sum != "" {
			if err := s.wake(ctx, tx, sess, "", sum, detail); err != nil {
				return false, err
			}
			if err := event(ctx, tx, sess, "intelligence", evKind, evLevel, sum,
				map[string]string{"proposalId": x.propID, "runId": x.runID}); err != nil {
				return false, err
			}
		}
	}
	return changed, nil
}

// activeWorkflowRunIDs returns all non-terminal workflow run IDs linked to active Grimm sessions
// so the supervisor can subscribe to their live change topics.
func (s *Service) activeWorkflowRunIDs(ctx context.Context) ([]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT r.id::text
		FROM grimm_workflow_runs g
		JOIN grimm_sessions s ON s.id = g.session_id
		JOIN workflow_proposals p ON p.id = g.proposal_id
		JOIN workflow_runs r ON r.id = p.started_run_id
		WHERE s.status = 'active' AND r.status NOT IN ('succeeded', 'failed', 'canceled')`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
