package web

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"github.com/zachlatta/intelligence/internal/workflow"
)

type proposalChange struct {
	Label, Before, After string
	Lines                []proposalDiffLine
}

func prettyProposal(v any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	_ = e.Encode(v)
	return b.String()
}

// Compare fields separately, matching steps by stable id. Revision numbers are
// audit metadata, not changes for the reviewer to wade through.
func proposalChanges(a, b *workflow.WorkflowState) []proposalChange {
	out := []proposalChange{}
	add := func(label, x, y string) {
		if x != y {
			out = append(out, proposalChange{label, x, y, proposalLineDiff(x, y)})
		}
	}
	if a == nil {
		a = &workflow.WorkflowState{}
	}
	if b == nil {
		b = &workflow.WorkflowState{}
	}
	add("Workflow name", a.Name, b.Name)
	add("Description", a.Description, b.Description)
	if a.Deleted != b.Deleted {
		add("Deleted", strconv.FormatBool(a.Deleted), strconv.FormatBool(b.Deleted))
	}
	if a.IsDefault != b.IsDefault {
		add("Default", strconv.FormatBool(a.IsDefault), strconv.FormatBool(b.IsDefault))
	}
	x, y := a.Definition, b.Definition
	add("Host", x.Host, y.Host)
	add("Trigger", x.Trigger, y.Trigger)
	add("Email filter", prettyProposal(x.Email), prettyProposal(y.Email))
	add("Voice memo filter", prettyProposal(x.VoiceMemo), prettyProposal(y.VoiceMemo))
	add("Project mode", x.Project.Mode, y.Project.Mode)
	add("Project directory", x.Project.Directory, y.Project.Directory)
	add("Project provider", x.Project.Provider, y.Project.Provider)
	add("Project model", x.Project.Model, y.Project.Model)
	add("Project thinking", x.Project.Thinking, y.Project.Thinking)
	add("Project instructions", x.Project.Instructions, y.Project.Instructions)
	add("Orchestrator provider", x.Orchestrator.Provider, y.Orchestrator.Provider)
	add("Orchestrator model", x.Orchestrator.Model, y.Orchestrator.Model)
	add("Orchestrator thinking", x.Orchestrator.Thinking, y.Orchestrator.Thinking)
	add("Orchestrator instructions", x.Orchestrator.Instructions, y.Orchestrator.Instructions)
	old := map[string]workflow.Step{}
	oldIndex := map[string]int{}
	newIDs := map[string]bool{}
	for i, s := range x.Steps {
		old[s.ID] = s
		oldIndex[s.ID] = i
	}
	for i, s := range y.Steps {
		newIDs[s.ID] = true
		prev, exists := old[s.ID]
		label := "Step “" + s.Title + "”: "
		if !exists {
			add(label+"added", "", s.Kind+"\n"+s.Instructions)
			continue
		}
		add(label+"title", prev.Title, s.Title)
		add(label+"kind", prev.Kind, s.Kind)
		add(label+"instructions", prev.Instructions, s.Instructions)
		if oldIndex[s.ID] != i {
			add(label+"position", strconv.Itoa(oldIndex[s.ID]+1), strconv.Itoa(i+1))
		}
	}
	for _, s := range x.Steps {
		if !newIDs[s.ID] {
			add("Step “"+s.Title+"”: removed", s.Kind+"\n"+s.Instructions, "")
		}
	}
	return out
}

type proposalView struct {
	ID, WorkflowID, Summary, Operation, Proposer, ProposedBy, RunID, Status, Reason, ReplacesID string
	Created, Expiry                                                                             time.Time
	TokenCreated                                                                                *time.Time
	Decided, Undone                                                                             *time.Time
	BeforeState, AfterState, AppliedState, UndoState                                            *workflow.WorkflowState
	Input, StartedRunID                                                                         string
	RawBefore, RawAfter                                                                         string
	Target, Dictation                                                                           string
	BaseVersion, ResultVersion                                                                  int
	Pending, Applied                                                                            bool
	Changes                                                                                     []proposalChange
	Comments                                                                                    []proposalComment
}

func (p proposalView) Headline() string {
	// Read visible text from the existing Markdown parser, preserving literal
	// punctuation inside code spans and resolving links to their labels. Parsing
	// the whole summary also resolves reference links defined on later lines.
	source := []byte(p.Summary)
	doc := md.Parser().Parse(text.NewReader(source))
	var line strings.Builder
	var block ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if block == nil && entering {
			switch n.(type) {
			case *ast.Paragraph, *ast.Heading, *ast.TextBlock:
				block = n
			}
		}
		if block == nil {
			return ast.WalkContinue, nil
		}
		if !entering {
			if n == block {
				if strings.TrimSpace(line.String()) != "" {
					return ast.WalkStop, nil
				}
				block = nil
			}
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Text:
			value := string(n.Value(source))
			if n.Parent() != nil && n.Parent().Kind() == ast.KindCodeSpan {
				line.WriteString(strings.ReplaceAll(value, "\n", " "))
			} else {
				line.WriteString(html.UnescapeString(string(util.UnescapePunctuations([]byte(value)))))
			}
			if (n.SoftLineBreak() || n.HardLineBreak()) && strings.TrimSpace(line.String()) != "" {
				return ast.WalkStop, nil
			}
		case *ast.String:
			line.Write(n.Value)
		case *ast.AutoLink:
			line.Write(n.Label(source))
		}
		return ast.WalkContinue, nil
	})
	if headline := strings.TrimSpace(line.String()); headline != "" {
		return headline
	}
	if p.Target != "" {
		return "Proposed " + p.Operation + " to " + p.Target
	}
	return "Proposed " + p.Operation
}

func (p proposalView) HasPendingAnswers() bool {
	for _, c := range p.Comments {
		if c.AnswerPending() {
			return true
		}
	}
	return false
}

type proposalComment struct {
	ID                                          int64
	Author, Text                                string
	Created                                     time.Time
	State, AnswerHost, AnswerAgent, AnswerError string
	AnswerLocation                              string
	AnswerOnline                                bool
}

func (c proposalComment) Retryable() bool { return c.State == "failed" || c.State == "timed_out" }

func (c proposalComment) AnswerPending() bool {
	return c.State == "queued" || c.State == "starting" || c.State == "answering"
}

func readProposalViews(ctx context.Context, v viewer, id, wid string) ([]proposalView, error) {
	rows, err := v.pool.Query(ctx, `SELECT id::text,coalesce(workflow_id::text,''),summary,operation,proposer,proposed_by,run_id,effective_status,
 withdraw_reason,coalesce(replaces_id::text,''),created_at,expires_at,decided_at,undone_at,before_state,proposed_state,after_state,undo_state,
 (SELECT t.created_at FROM mcp_tokens t WHERE 'mcp:' || t.id::text = proposer) AS token_created_at,input,coalesce(started_run_id::text,'')
 FROM workflow_proposal_status WHERE ($1='' OR id::text=$1) AND ($2='' OR workflow_id::text=$2)
 ORDER BY (effective_status='pending') DESC,created_at DESC,id LIMIT 200`, id, wid)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (proposalView, error) {
		var p proposalView
		var before, after, applied, undo []byte
		err := row.Scan(&p.ID, &p.WorkflowID, &p.Summary, &p.Operation, &p.Proposer, &p.ProposedBy, &p.RunID, &p.Status, &p.Reason, &p.ReplacesID, &p.Created, &p.Expiry, &p.Decided, &p.Undone, &before, &after, &applied, &undo, &p.TokenCreated, &p.Input, &p.StartedRunID)
		if err != nil {
			return p, err
		}
		for _, pair := range []struct {
			data   []byte
			target **workflow.WorkflowState
		}{{before, &p.BeforeState}, {after, &p.AfterState}, {applied, &p.AppliedState}, {undo, &p.UndoState}} {
			if len(pair.data) > 0 {
				if err := json.Unmarshal(pair.data, pair.target); err != nil {
					return p, err
				}
			}
		}
		p.RawBefore = prettyProposal(p.BeforeState)
		p.RawAfter = prettyProposal(p.AfterState)
		p.Pending = p.Status == "pending"
		p.Applied = p.Status == "applied" && p.Operation != "run"
		if p.BeforeState != nil {
			p.Target = p.BeforeState.Name
			p.BaseVersion = p.BeforeState.Version
		} else if p.AfterState != nil {
			p.Target = p.AfterState.Name
		}
		if p.AppliedState != nil && p.Operation != "run" {
			p.ResultVersion = p.AppliedState.Version
		}
		if p.UndoState != nil {
			p.ResultVersion = p.UndoState.Version
		}
		p.Changes = proposalChanges(p.BeforeState, p.AfterState)
		return p, nil
	})
}

func proposalPage(cfg Config, v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !workflowIDRe.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		ps, err := readProposalViews(r.Context(), v, id, "")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if len(ps) == 0 {
			http.NotFound(w, r)
			return
		}
		rows, err := v.pool.Query(r.Context(), `SELECT c.id,c.author,c.text,c.created_at,coalesce(q.state,''),coalesce(q.host,''),coalesce(q.agent_id,''),coalesce(q.error,''),coalesce(h.online,false),coalesce(q.answer_location,'')
 FROM workflow_proposal_comments c LEFT JOIN workflow_proposal_question_status q ON q.question_id=c.id
 LEFT JOIN host_status h ON h.name=q.host WHERE c.proposal_id=$1 ORDER BY c.id`, id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		ps[0].Comments, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (proposalComment, error) {
			var c proposalComment
			err := row.Scan(&c.ID, &c.Author, &c.Text, &c.Created, &c.State, &c.AnswerHost, &c.AnswerAgent, &c.AnswerError, &c.AnswerOnline, &c.AnswerLocation)
			return c, err
		})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		ps[0].Dictation = dictationPath(cfg)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = proposalTmpl.ExecuteTemplate(w, "proposal", ps[0])
	}
}

func proposalsPage(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := r.URL.Query().Get("workflow")
		if wid != "" && !workflowIDRe.MatchString(wid) {
			http.NotFound(w, r)
			return
		}
		ps, err := readProposalViews(r.Context(), v, "", wid)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = proposalTmpl.ExecuteTemplate(w, "proposals", ps)
	}
}

// Version links read the immutable saved definition, including soft-deleted workflows.
func workflowVersionPage(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		n, err := strconv.Atoi(r.PathValue("version"))
		if err != nil || n < 1 || !workflowIDRe.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		var s workflow.WorkflowState
		var def []byte
		err = v.pool.QueryRow(r.Context(), `SELECT w.name,v.version,v.definition FROM workflows w JOIN workflow_versions v ON v.workflow_id=w.id WHERE w.id=$1 AND v.version=$2`, id, n).Scan(&s.Name, &s.Version, &def)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if err = json.Unmarshal(def, &s.Definition); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = proposalTmpl.ExecuteTemplate(w, "workflow-version", s)
	}
}

//go:embed proposals.html
var proposalsHTML string
var proposalTmpl = template.Must(template.New("proposals").Funcs(template.FuncMap{"markdown": markdown, "inlineDiff": proposalInlineDiff, "stamp": func(t time.Time) string { return t.Format("Jan 2, 2006 · 15:04 MST") }}).Parse(proposalsHTML + layoutHTML))
