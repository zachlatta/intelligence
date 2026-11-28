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
	if len(input) > 1<<20 {
		return "", badRequest("workflow request input is longer than 1 MiB")
	}
	var who Caller
	var sess Session
	var wfID, wfName, wfHost string
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
		return err
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
		msgID, err := threadRow(ctx, tx, locked, "workflow_request", ManagerName, summary, map[string]any{
			"proposalId":   propID,
			"workflowId":   wfID,
			"workflowName": wfName,
			"host":         wfHost,
		})
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
	return fmt.Sprintf("requested workflow %s (%s): proposal %s is pending approval in the web UI (no run started yet)", wfName, wfHost, propID), nil
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
	if input == "" || len(input) > 1<<20 {
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
			"Edited task input for pending workflow request "+proposalID[:8],
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
		line := fmt.Sprintf("%s  %s (%s)  proposal=%s", pid[:8], wfName, host, pStatus)
		if runID != "" {
			line += fmt.Sprintf("  run=%s status=%s", runID[:8], rStatus)
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
	var pid, wfName, host, summary, input, pStatus, runID, rStatus, stepTitle, stepKind, note, rErr, waitingOn string
	var step, stepCount int
	var stepsRaw []byte
	err := tx.QueryRow(ctx, `SELECT proposal_id::text, workflow_name, host, summary, input, proposal_status,
		coalesce(run_id::text, ''), run_status, run_step, step_count, step_title, step_kind,
		run_note, run_error, waiting_on, steps
		FROM grimm_workflow_run_status
		WHERE session_id = $1 AND (
			proposal_id::text = $2 OR proposal_id::text LIKE $2 || '%'
			OR run_id::text = $2 OR run_id::text LIKE $2 || '%'
		)
		ORDER BY created_at DESC LIMIT 1`, sessionID, target).Scan(
		&pid, &wfName, &host, &summary, &input, &pStatus,
		&runID, &rStatus, &step, &stepCount, &stepTitle, &stepKind,
		&note, &rErr, &waitingOn, &stepsRaw,
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
		if note != "" && rStatus == "waiting" {
			fmt.Fprintf(&b, "Question: %s\n", cut(note, 500))
		}
		if rErr != "" {
			fmt.Fprintf(&b, "Error:    %s\n", cut(rErr, 500))
		}
	}
	fmt.Fprintf(&b, "Input:\n%s", cut(input, 1000))
	return b.String(), nil
}

// reconcileWorkflowRuns checks every workflow request in sess for proposal decisions and run
// milestones, updating grimm_workflow_runs and queueing debounced "[Grimm update]" wakeups.
func (s *Service) reconcileWorkflowRuns(ctx context.Context, tx pgx.Tx, sess Session) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT g.proposal_id::text, g.last_proposal_status, g.last_run_status, g.last_run_step, g.last_run_note,
		s.workflow_name, s.proposal_status, coalesce(s.run_id::text, ''), s.run_status, s.run_step, s.step_count, s.step_title, s.step_kind,
		s.run_note, s.run_error, s.waiting_on
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
		wfName, propStatus, runID           string
		runStatus                           string
		runStep, stepCount                  int
		stepTitle, stepKind                 string
		runNote, runError, waitingOn        string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.propID, &x.lastProp, &x.lastRun, &x.lastStep, &x.lastNote,
			&x.wfName, &x.propStatus, &x.runID, &x.runStatus, &x.runStep, &x.stepCount, &x.stepTitle, &x.stepKind,
			&x.runNote, &x.runError, &x.waitingOn)
	})
	if err != nil {
		return false, err
	}
	changed := false
	for _, x := range list {
		propChanged := x.propStatus != x.lastProp
		runChanged := x.runID != "" && (x.runStatus != x.lastRun || x.runStep != x.lastStep || (x.runStatus == "waiting" && x.runNote != x.lastNote))
		if !propChanged && !runChanged {
			continue
		}
		changed = true
		if _, err := tx.Exec(ctx, `UPDATE grimm_workflow_runs
			SET last_proposal_status = $2, last_run_status = $3, last_run_step = $4, last_run_note = $5
			WHERE proposal_id = $1`, x.propID, x.propStatus, x.runStatus, x.runStep, x.runNote); err != nil {
			return false, err
		}

		// Proposal decision wakeup (when rejected/expired/stale/withdrawn, or approved without run update below).
		if propChanged && x.propStatus != "pending" && x.runID == "" {
			sum := fmt.Sprintf("workflow %s %s", x.wfName, x.propStatus)
			detail := fmt.Sprintf("Workflow request **%s** (proposal `%s`) was **%s**.", x.wfName, x.propID[:8], x.propStatus)
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
		shortRun := x.runID
		if len(shortRun) > 8 {
			shortRun = shortRun[:8]
		}
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
			detail = fmt.Sprintf("Workflow **%s** was approved by Zach and started (run `%s`, status: `%s`", x.wfName, shortRun, x.runStatus)
			if stepLabel != "" {
				detail += ", " + stepLabel
			}
			detail += ")."
		case x.runStatus == "waiting":
			evKind = "workflow_waiting"
			evLevel = "warn"
			sum = fmt.Sprintf("workflow %s waiting for Zach", x.wfName)
			detail = fmt.Sprintf("Workflow **%s** (run `%s`) is **waiting for Zach**", x.wfName, shortRun)
			if stepLabel != "" {
				detail += " at " + stepLabel
			}
			if x.runNote != "" {
				detail += ":\n\n> " + strings.ReplaceAll(cut(x.runNote, 600), "\n", "\n> ")
			} else if x.waitingOn != "" {
				detail += " (" + x.waitingOn + ")."
			} else {
				detail += "."
			}
		case x.runStatus == "succeeded":
			evKind = "workflow_succeeded"
			sum = fmt.Sprintf("workflow %s succeeded", x.wfName)
			detail = fmt.Sprintf("Workflow **%s** (run `%s`) **succeeded**.", x.wfName, shortRun)
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

// Ensure time import is used if needed in future.
var _ = time.Second
