package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zachlatta/intelligence/internal/attachment"
)

// AgentHeader names the calling Paseo agent (PASEO_AGENT_ID) on host-key CLI requests.
const AgentHeader = "X-Paseo-Agent-Id"

// APIActor identifies the scoped workflow API key performing an action.
type APIActor struct {
	KeyID   string
	KeyName string
}

type apiActorKey struct{}

// WithAPIActor attaches an authenticated workflow API key to ctx so engine
// events, messages and run attributions record the acting key.
func WithAPIActor(ctx context.Context, actor APIActor) context.Context {
	if actor.KeyID == "" {
		return ctx
	}
	return context.WithValue(ctx, apiActorKey{}, actor)
}

func apiActorFrom(ctx context.Context) APIActor {
	actor, _ := ctx.Value(apiActorKey{}).(APIActor)
	return actor
}

// GrimmActor identifies a Grimm session manager sending mid-run guidance,
// clarification replies, or cancellations to a linked workflow run.
type GrimmActor struct {
	SessionID string
	AgentID   string
}

type grimmActorKey struct{}

// WithGrimmActor attaches a Grimm session actor to ctx so workflow_run_events
// record grimm_session_id and messages clearly attribute Grimm (not the human).
func WithGrimmActor(ctx context.Context, actor GrimmActor) context.Context {
	if actor.SessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, grimmActorKey{}, actor)
}

func grimmActorFrom(ctx context.Context) GrimmActor {
	actor, _ := ctx.Value(grimmActorKey{}).(GrimmActor)
	return actor
}

func shortGrimmSession(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

type callerAgentKey struct{}

// WithCallerAgentID attaches the caller's PASEO_AGENT_ID to ctx so MoveRun can
// refuse Grimm agents attempting to drive workflow state transitions directly.
func WithCallerAgentID(ctx context.Context, agentID string) context.Context {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return ctx
	}
	return context.WithValue(ctx, callerAgentKey{}, agentID)
}

func callerAgentIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(callerAgentKey{}).(string)
	return id
}

func nullUUID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// APIAttachment is an inline file attached to an API-triggered run, reply or input.
type APIAttachment struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// APIRunRequest is what an API-key caller sends to trigger a workflow run.
type APIRunRequest struct {
	Title          string          `json:"title,omitempty"`
	Input          string          `json:"input"`
	Attachments    []APIAttachment `json:"attachments,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
}

type explicitTitleKey struct{}

func withExplicitTitle(ctx context.Context, title string) context.Context {
	if title == "" {
		return ctx
	}
	return context.WithValue(ctx, explicitTitleKey{}, title)
}

func explicitTitleFrom(ctx context.Context) string {
	t, _ := ctx.Value(explicitTitleKey{}).(string)
	return t
}

func cleanAPITitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return "", nil
	}
	if utf8.RuneCountInString(title) > 80 || strings.ContainsFunc(raw, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Cf, r)
	}) {
		return "", bad("title must be one line of 1–80 characters")
	}
	return title, nil
}

// APIRunResult distinguishes a newly started run from an idempotent replay (Replayed=true).
type APIRunResult struct {
	RunID    string
	Replayed bool
}

// MaxAPITextBytes caps text fields (input, reply/input text) at 1 MiB so no
// queued host command can exceed hostclient.MaxFrame.
const MaxAPITextBytes = 1 << 20

func cleanIdempotencyKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}
	if len(key) > 255 {
		return "", bad("idempotency key must be at most 255 characters")
	}
	if strings.IndexFunc(key, func(r rune) bool { return unicode.IsControl(r) || r > 127 }) >= 0 {
		return "", bad("idempotency key must contain printable ASCII characters only")
	}
	return key, nil
}

func apiRequestHash(title, input string, atts []APIAttachment) string {
	h := sha256.New()
	_, _ = h.Write([]byte(input))
	_, _ = h.Write([]byte{0})
	for _, a := range atts {
		_, _ = h.Write([]byte(strings.TrimSpace(a.Name)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(a.Data)
		_, _ = h.Write([]byte{0})
	}
	if title != "" {
		_, _ = h.Write([]byte("title"))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(title))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AppendAttachments validates and stores inline attachments in tx, appending [attachment:<id>]
// references to text in the same format the web UI uses.
func AppendAttachments(ctx context.Context, tx pgx.Tx, account int64, text string, atts []APIAttachment) (string, error) {
	if len(atts) == 0 {
		return text, nil
	}
	if len(atts) > attachment.MaxCount {
		return "", bad("attach at most %d files, %d MiB total", attachment.MaxCount, attachment.MaxTotal>>20)
	}
	refs := make([]string, 0, len(atts))
	total := 0
	for _, a := range atts {
		name := strings.TrimSpace(a.Name)
		if name == "" || len(name) > 255 {
			return "", bad("attachment name must be 1–255 bytes")
		}
		total += len(a.Data)
		if total > attachment.MaxTotal {
			return "", bad("attach at most %d files, %d MiB total", attachment.MaxCount, attachment.MaxTotal>>20)
		}
		media, err := attachment.ValidateFile(name, a.Data)
		if err != nil {
			return "", bad("%s", err)
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO attachments(account_id,name,media_type,data) VALUES($1,$2,$3,$4) RETURNING id::text`,
			account, name, media, a.Data).Scan(&id); err != nil {
			return "", err
		}
		refs = append(refs, "[attachment:"+id+"]")
	}
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return strings.Join(refs, "\n"), nil
	}
	return trimmed + "\n\n" + strings.Join(refs, "\n"), nil
}

// StartAPIRun triggers a run of workflowID using an authenticated API key. It enforces
// that the workflow's apiTrigger setting is enabled, stores optional attachments in the
// same transaction, records api_key_id, and supports idempotent retries via idempotencyKey.
func (e *Engine) StartAPIRun(ctx context.Context, account int64, workflowID, keyID string, req APIRunRequest) (APIRunResult, error) {
	title, err := cleanAPITitle(req.Title)
	if err != nil {
		return APIRunResult{}, err
	}
	idemKey, err := cleanIdempotencyKey(req.IdempotencyKey)
	if err != nil {
		return APIRunResult{}, err
	}
	if len(req.Input) > MaxAPITextBytes {
		return APIRunResult{}, bad("input exceeds 1 MiB limit")
	}
	if strings.ContainsRune(req.Input, 0) {
		return APIRunResult{}, bad("input cannot contain a NUL character")
	}
	if strings.TrimSpace(req.Input) == "" {
		return APIRunResult{}, bad("a run needs input: what should be done")
	}
	if !uuidRe.MatchString(workflowID) {
		return APIRunResult{}, ErrNotFound
	}
	var apiEnabledPre bool
	if err := e.p.QueryRow(ctx, `SELECT coalesce((v.definition->>'apiTrigger')::boolean, false)
		FROM workflows w JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
		WHERE w.id = $1 AND w.account_id = $2 AND w.deleted_at IS NULL`,
		workflowID, account).Scan(&apiEnabledPre); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return APIRunResult{}, ErrNotFound
		}
		return APIRunResult{}, err
	}
	if !apiEnabledPre {
		return APIRunResult{}, conflict("API trigger is disabled for this workflow")
	}
	reqHash := ""
	if idemKey != "" {
		reqHash = apiRequestHash(title, req.Input, req.Attachments)
		var existingID, existingHash string
		if qerr := e.p.QueryRow(ctx, `SELECT id::text, idempotency_hash FROM workflow_runs
			WHERE workflow_id = $1 AND idempotency_key = $2`, workflowID, idemKey).Scan(&existingID, &existingHash); qerr == nil {
			if existingHash != reqHash {
				return APIRunResult{}, conflict("idempotency key already used with different request parameters")
			}
			return APIRunResult{RunID: existingID, Replayed: true}, nil
		}
	}
	ctx, err = e.prepareVerifiedTree(ctx, account, workflowID, req.Input, "")
	if err != nil {
		return APIRunResult{}, err
	}
	ctx, flushPushes := e.CollectPushes(ctx)
	var out APIRunResult
	err = pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		var apiEnabled bool
		err := tx.QueryRow(ctx, `SELECT coalesce((v.definition->>'apiTrigger')::boolean, false)
			FROM workflows w JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
			WHERE w.id = $1 AND w.account_id = $2 AND w.deleted_at IS NULL FOR SHARE OF w`,
			workflowID, account).Scan(&apiEnabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !apiEnabled {
			return conflict("API trigger is disabled for this workflow")
		}
		if idemKey != "" {
			var existingID, existingHash string
			err := tx.QueryRow(ctx, `SELECT id::text, idempotency_hash FROM workflow_runs
				WHERE workflow_id = $1 AND idempotency_key = $2`, workflowID, idemKey).Scan(&existingID, &existingHash)
			if err == nil {
				if existingHash != reqHash {
					return conflict("idempotency key already used with different request parameters")
				}
				out = APIRunResult{RunID: existingID, Replayed: true}
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		fullInput, err := AppendAttachments(ctx, tx, account, req.Input, req.Attachments)
		if err != nil {
			return err
		}
		runID, err := e.StartRunTx(withExplicitTitle(ctx, title), tx, account, workflowID, fullInput, time.Time{})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET api_key_id = $2, idempotency_key = $3, idempotency_hash = $4 WHERE id = $1`,
			runID, keyID, idemKey, reqHash); err != nil {
			return err
		}
		out = APIRunResult{RunID: runID, Replayed: false}
		return nil
	})
	if err != nil {
		var pg *pgconn.PgError
		if idemKey != "" && errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "workflow_runs_api_idempotency" {
			var existingID, existingHash string
			qerr := e.p.QueryRow(ctx, `SELECT id::text, idempotency_hash FROM workflow_runs
				WHERE workflow_id = $1 AND idempotency_key = $2`, workflowID, idemKey).Scan(&existingID, &existingHash)
			if qerr != nil {
				return APIRunResult{}, qerr
			}
			if existingHash != reqHash {
				return APIRunResult{}, conflict("idempotency key already used with different request parameters")
			}
			return APIRunResult{RunID: existingID, Replayed: true}, nil
		}
		return APIRunResult{}, err
	}
	if !out.Replayed {
		e.sendFailureAlertsBestEffort(ctx)
		flushPushes()
	}
	return out, nil
}

// ReplyWithAttachments stores optional inline attachments and sends a reply in one transaction,
// so a refused reply (e.g. 409) leaves no orphaned attachments.
func (e *Engine) ReplyWithAttachments(ctx context.Context, account int64, runID, text string, atts []APIAttachment) error {
	if !uuidRe.MatchString(runID) {
		return ErrNotFound
	}
	if len(text) > MaxAPITextBytes {
		return bad("text exceeds 1 MiB limit")
	}
	if strings.ContainsRune(text, 0) {
		return bad("a reply cannot contain a NUL character")
	}
	if strings.TrimSpace(text) == "" {
		return bad("a reply needs text")
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		fullText, err := AppendAttachments(ctx, tx, account, text, atts)
		if err != nil {
			return err
		}
		return e.ReplyTx(ctx, tx, account, runID, fullText)
	})
}

// AddInputWithAttachments stores optional inline attachments and sends follow-up input in one
// transaction, so a refused request (e.g. 409) leaves no orphaned attachments.
func (e *Engine) AddInputWithAttachments(ctx context.Context, account int64, runID, text string, atts []APIAttachment) error {
	if !uuidRe.MatchString(runID) {
		return ErrNotFound
	}
	if len(text) > MaxAPITextBytes {
		return bad("text exceeds 1 MiB limit")
	}
	if strings.ContainsRune(text, 0) {
		return bad("additional input cannot contain a NUL character")
	}
	if strings.TrimSpace(text) == "" {
		return bad("additional input needs text")
	}
	return e.forAccount(ctx, account, runID, func(tx pgx.Tx, r run) error {
		fullText, err := AppendAttachments(ctx, tx, account, text, atts)
		if err != nil {
			return err
		}
		return e.addInputLocked(ctx, tx, account, r, fullText)
	})
}
