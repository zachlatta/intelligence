package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ModelGroup is a shared allowlist. Token is immutable, including after a rename.
type ModelGroup struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Token    string        `json:"token"`
	Revision int           `json:"revision"`
	Members  []ModelMember `json:"members"`
}

type ModelMember struct {
	Provider      string     `json:"provider"`
	Model         string     `json:"model"`
	Thinking      string     `json:"thinking"`
	Login         string     `json:"login"`
	LongWindow    string     `json:"longWindow"`
	EligibleUntil *time.Time `json:"eligibleUntil"`
	Enabled       bool       `json:"enabled"`
}

var tokenWords = regexp.MustCompile(`[^a-z0-9]+`)
var modelSetting = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]{0,159}$`)
var groupToken = regexp.MustCompile(`\{\{([a-z][a-z0-9_]*_model)\}\}`)

func tokenBase(name string) string {
	s := strings.Trim(tokenWords.ReplaceAllString(strings.ToLower(name), "_"), "_")
	if len(s) > 48 {
		s = strings.TrimRight(s[:48], "_")
	}
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "group_" + s
	}
	return strings.TrimRight(s, "_")
}

// SaveModelGroup creates or replaces a group. Account locking serializes edits, workflow
// reference checks and allocations; an editor's revision prevents overwriting another draft.
func (e *Engine) SaveModelGroup(ctx context.Context, account int64, id string, g ModelGroup) (ModelGroup, error) {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" || len(g.Name) > 100 {
		return g, bad("a model group needs a name, up to 100 characters")
	}
	if len(g.Members) > 100 {
		return g, bad("at most 100 members per group")
	}
	if id != "" && !uuidRe.MatchString(id) {
		return g, ErrNotFound
	}
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		if err := lockModels(ctx, tx, account); err != nil {
			return err
		}
		var duplicate bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_groups
            WHERE account_id=$1 AND deleted_at IS NULL AND lower(name)=lower($2) AND id::text<>$3)`, account, g.Name, id).Scan(&duplicate); err != nil {
			return err
		}
		if duplicate {
			return conflict("A model group with this name already exists. Choose a different name.")
		}
		if id == "" {
			base := tokenBase(g.Name)
			for n := 1; ; n++ {
				g.Token = base + "_model"
				if n > 1 {
					g.Token = fmt.Sprintf("%s_%d_model", base, n)
				}
				var taken bool
				if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM model_groups WHERE account_id=$1 AND token=$2)", account, g.Token).Scan(&taken); err != nil {
					return err
				}
				if !taken {
					break
				}
			}
			if err := tx.QueryRow(ctx, `INSERT INTO model_groups(account_id,name,token) VALUES($1,$2,$3) RETURNING id::text,revision`, account, g.Name, g.Token).Scan(&g.ID, &g.Revision); err != nil {
				return err
			}
		} else {
			var revision int
			if err := tx.QueryRow(ctx, `SELECT token,revision FROM model_groups WHERE id=$1 AND account_id=$2 AND deleted_at IS NULL`, id, account).Scan(&g.Token, &revision); errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			} else if err != nil {
				return err
			}
			if revision != g.Revision {
				return conflict("group changed; reload before saving")
			}
			g.ID = id
			g.Revision++
			if _, err := tx.Exec(ctx, "UPDATE model_groups SET name=$3,revision=$4 WHERE id=$1 AND account_id=$2", id, account, g.Name, g.Revision); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "DELETE FROM model_group_members WHERE group_id=$1", id); err != nil {
				return err
			}
		}
		for i, m := range g.Members {
			if strings.TrimSpace(m.Login) == "" {
				return bad("Choose a plan/login for each model.")
			}
			if !modelSetting.MatchString(m.Provider) || !modelSetting.MatchString(m.Model) || (m.Thinking != "" && !modelSetting.MatchString(m.Thinking)) {
				return bad("invalid model launch settings")
			}
			var valid bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM provider_models p JOIN host_keys k ON k.host=p.host
     WHERE k.account_id=$1 AND p.provider=$2 AND p.id=$3 AND ((cardinality(p.thinking)=0 AND $4='') OR $4=ANY(p.thinking)))`, account, m.Provider, m.Model, m.Thinking).Scan(&valid)
			if err != nil {
				return err
			}
			if !valid {
				return bad("model or thinking level is not in an owned host's catalog")
			}
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_sources s JOIN host_keys k ON k.host=s.host
      JOIN usage_accounts a ON a.provider=s.provider AND a.account=s.account
      WHERE k.account_id=$1 AND s.provider=$2 AND s.account=$3)`, account, m.Provider, m.Login).Scan(&valid)
			if err != nil {
				return err
			}
			if !valid {
				return bad("plan login is not one of this account's observed logins")
			}
			if err := checkQuotaPool(ctx, tx, &m); err != nil {
				return err
			}
			g.Members[i] = m

			if _, err := tx.Exec(ctx, `INSERT INTO model_group_members(account_id,group_id,position,provider,model,thinking,login,long_window,eligible_until,enabled)
     VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, account, g.ID, i, m.Provider, m.Model, m.Thinking, m.Login, m.LongWindow, m.EligibleUntil, m.Enabled); err != nil {
				return err
			}
		}
		return nil
	})
	return g, err
}

// checkQuotaPool checks m's pool against usage, not model-name conventions: the user chooses the
// longest observed window of a pool, and the candidates view reads its shorter windows live.
// A disabled entry with no observed window is kept without one until usage is available.
func checkQuotaPool(ctx context.Context, tx pgx.Tx, m *ModelMember) error {
	rows, err := tx.Query(ctx, `SELECT window_id,coalesce(window_seconds,0) FROM usage_current
        WHERE provider=$1 AND account=$2`, m.Provider, m.Login)
	if err != nil {
		return err
	}
	type window struct {
		ID      string
		Seconds int
	}
	windows, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (window, error) {
		var w window
		err := row.Scan(&w.ID, &w.Seconds)
		return w, err
	})
	if err != nil {
		return err
	}
	seconds := 0
	for _, w := range windows {
		if w.ID == m.LongWindow {
			seconds = w.Seconds
		}
	}
	if seconds <= 0 {
		if !m.Enabled {
			m.LongWindow = ""
			return nil
		}
		return bad("Choose an observed quota pool, or disable this entry until usage is available.")
	}
	pool := func(id string) string { return id[:strings.LastIndexByte(id, '/')+1] }
	for _, w := range windows {
		if pool(w.ID) == pool(m.LongWindow) && w.Seconds > seconds {
			return bad("Choose the longest observed window in this quota pool.")
		}
	}
	return nil
}

func lockModels(ctx context.Context, tx pgx.Tx, account int64) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", account)
	return err
}

func (e *Engine) DeleteModelGroup(ctx context.Context, account int64, id string) error {
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		if err := lockModels(ctx, tx, account); err != nil {
			return err
		}
		var token string
		if err := tx.QueryRow(ctx, "SELECT token FROM model_groups WHERE id=$1 AND account_id=$2 AND deleted_at IS NULL", id, account).Scan(&token); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_versions v WHERE v.account_id=$1 AND position($2 in v.definition::text)>0
    AND (EXISTS(SELECT 1 FROM workflows w WHERE w.id=v.workflow_id AND w.version=v.version AND w.deleted_at IS NULL)
      OR EXISTS(SELECT 1 FROM workflow_runs r WHERE r.workflow_id=v.workflow_id AND r.version=v.version AND r.finished_at IS NULL)))`, account, "{{"+token+"}}").Scan(&used); err != nil {
			return err
		}
		if used {
			return conflict("group token is used by a workflow or active run; remove those references first")
		}
		_, err := tx.Exec(ctx, "UPDATE model_groups SET deleted_at=now() WHERE id=$1", id)
		return err
	})
}

func validateModelTokens(ctx context.Context, tx pgx.Tx, account int64, d Definition) error {
	if err := lockModels(ctx, tx, account); err != nil {
		return err
	}
	for _, s := range d.Steps {
		for _, m := range groupToken.FindAllStringSubmatch(s.Instructions, -1) {
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM model_groups WHERE account_id=$1 AND token=$2 AND deleted_at IS NULL)", account, m[1]).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return bad("unknown model token %s in step %s", m[0], s.Title)
			}
		}
	}
	return nil
}

// fillModels operates on saved instructions before user input is interpolated. A token
// in task text is literal; repeated tokens in one delivery share a single allocation.
func (e *Engine) fillModels(ctx context.Context, tx pgx.Tx, r run, step int) (Definition, error) {
	d := r.Def
	d.Steps = append([]Step(nil), d.Steps...)
	text := d.Steps[step].Instructions
	seen := map[string]bool{}
	var notes []string
	for _, m := range groupToken.FindAllStringSubmatch(text, -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		msg, err := e.suggestModel(ctx, tx, r, step, m[1], nil)
		if err != nil {
			return d, err
		}
		text = strings.ReplaceAll(text, m[0], msg.Inline)
		notes = append(notes, "- "+m[1]+": "+msg.Note)
	}
	if len(notes) > 0 {
		text += "\n\nModel suggestions:\n" + strings.Join(notes, "\n")
	}
	d.Steps[step].Instructions = text
	return d, nil
}

type modelSuggestion struct{ Inline, Note string }

func (e *Engine) suggestModel(ctx context.Context, tx pgx.Tx, r run, step int, token string, exclude []string) (modelSuggestion, error) {
	if err := lockModels(ctx, tx, r.Account); err != nil {
		return modelSuggestion{}, err
	}
	var group string
	var revision int
	if err := tx.QueryRow(ctx, "SELECT id::text,revision FROM model_groups WHERE account_id=$1 AND token=$2 AND deleted_at IS NULL", r.Account, token).Scan(&group, &revision); errors.Is(err, pgx.ErrNoRows) {
		return modelSuggestion{}, bad("unknown model token %s", token)
	} else if err != nil {
		return modelSuggestion{}, err
	}
	rows, err := tx.Query(ctx, `SELECT to_jsonb(c) || jsonb_build_object(
        'model_label',coalesce(nullif(p.label,''),c.model), 'login_label',coalesce(nullif(a.label,''),c.login))
        FROM model_group_candidates c
        LEFT JOIN provider_models p ON p.host=c.host AND p.provider=c.provider AND p.id=c.model
        LEFT JOIN usage_accounts a ON a.provider=c.provider AND a.account=c.login
        WHERE c.account_id=$1 AND c.group_id=$2 AND c.host=$3
        ORDER BY c.eligible DESC,c.score DESC NULLS LAST,c.position,c.id`, r.Account, group, r.Host)
	if err != nil {
		return modelSuggestion{}, err
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowTo[json.RawMessage])
	if err != nil {
		return modelSuggestion{}, err
	}
	var chosen map[string]any
	for _, raw := range candidates {
		var c map[string]any
		if err := json.Unmarshal(raw, &c); err != nil {
			return modelSuggestion{}, err
		}
		skip := false
		for _, x := range exclude {
			if c["provider"].(string)+"/"+c["model"].(string) == x {
				skip = true
			}
		}
		if c["eligible"] == true && !skip {
			chosen = c
			break
		}
	}
	msg := "No eligible model in " + token + ". Wait for a fresh meter or reset, or ask for an explicit model. Do not launch a worker on this suggestion."
	inline := "no eligible model in " + token
	var provider, login, window any
	if chosen != nil {
		p := chosen["provider"].(string)
		model := chosen["model"].(string)
		thinking := chosen["thinking"].(string)
		inline = chosen["model_label"].(string)
		if thinking != "" {
			inline += " (" + strings.ToUpper(thinking[:1]) + thinking[1:] + ")"
		}
		inline += " via " + p + "/" + model
		if thinking != "" {
			inline += " --thinking " + thinking
		}
		inline += " on " + chosen["login_label"].(string)
		msg = fmt.Sprintf("%s/%s", p, model)
		if thinking != "" {
			msg += " (" + thinking + ")"
		}
		msg += ". Launch settings: --provider " + p + "/" + model + " --mode " + agentMode(p)
		if thinking != "" {
			msg += " --thinking " + thinking
		}
		msg += ". More quota likely to remain at reset; plan " + chosen["login"].(string) + "; meter read " + chosen["last_observed"].(string) + ". Use these settings over generic model lists unless the task explicitly names another model."
		provider = p
		login = chosen["login"]
		window = chosen["long_window"]
	}
	snapshot, _ := json.Marshal(map[string]any{"candidates": candidates, "selected": chosen, "excluded": exclude})
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO workflow_model_suggestions(account_id,run_id,step,group_id,token,revision,provider,login,long_window,snapshot,message)
   VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id::text`, r.Account, r.ID, step, group, token, revision, provider, login, window, snapshot, msg).Scan(&id); err != nil {
		return modelSuggestion{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_run_events (run_id, account_id, at, kind, step, text, suggestion_id)
		VALUES ($1, $2, $3, 'model_suggestion', $4, $5, $6)`, r.ID, r.Account, e.Now(), step, msg, id); err != nil {
		return modelSuggestion{}, err
	}
	return modelSuggestion{Inline: inline, Note: msg + " Suggestion " + id + ". Before another worker, refresh with intelligence workflow suggest --run " + r.ID + " " + token + "."}, nil
}

// SuggestModel records a new allocation without advancing the step.
func (e *Engine) SuggestModel(ctx context.Context, host, id, token string, exclude []string) (string, error) {
	if !uuidRe.MatchString(id) {
		return "", ErrNotFound
	}
	var msg string
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, runSelect+"WHERE r.id=$1 AND r.host=$2 FOR UPDATE OF r", id, host))
		if err != nil {
			return err
		}
		if r.Status != Running {
			return conflict("run is not running")
		}
		value, err := e.suggestModel(ctx, tx, r, r.Step, token, exclude)
		msg = value.Inline + "\n\n" + value.Note
		return err
	})
	return msg, err
}
