package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/grimm"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

const grimmFileReadTTL = 30 * time.Second

// grimmReadFile is POST /api/grimm/sessions/{id}/read-file {"path": "..."}: validates that path
// lives inside the session's workspace or repository, queues a read_file command on the session's
// host, and returns {"commandId", "path", "name"} so the browser can read the result from commands
// over /api/sql.
func (a *app) grimmReadFile(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		if !decodeGrimm(w, r, &req) {
			return
		}
		cmdID, cleanPath, err := a.enqueueGrimmReadFile(r.Context(), v, r.PathValue("id"), req.Path)
		if err != nil {
			grimmError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]string{
			"commandId": cmdID,
			"path":      cleanPath,
			"name":      path.Base(cleanPath),
		})
	}
}

func (a *app) enqueueGrimmReadFile(ctx context.Context, v viewer, sessionID, rawPath string) (string, string, error) {
	if !grimm.ValidID(sessionID) {
		return "", "", grimm.ErrNotFound
	}
	cleanPath, err := cleanRequestedFilePath(rawPath)
	if err != nil {
		return "", "", &grimm.Error{Status: http.StatusBadRequest, Msg: err.Error()}
	}

	var host, repo, workspaceID, workspaceDir string
	var online bool
	err = v.pool.QueryRow(ctx, `SELECT s.host, s.repo, s.workspace_id, coalesce(w.directory, ''), coalesce(h.online, false)
		FROM grimm_sessions s
		LEFT JOIN workspaces w ON w.host = s.host AND w.id = s.workspace_id AND s.workspace_id <> ''
		LEFT JOIN host_status h ON h.name = s.host
		WHERE s.id = $1`, sessionID).Scan(&host, &repo, &workspaceID, &workspaceDir, &online)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", grimm.ErrNotFound
	}
	if err != nil {
		return "", "", err
	}

	roots := collectSessionRoots(workspaceDir, repo)
	// Also include any recorded agent cwd for this session (e.g. if the workspace row was unlisted/archived).
	rows, err := v.pool.Query(ctx, `SELECT DISTINCT a.cwd FROM agents a
		WHERE a.host = $1 AND a.cwd <> '' AND (
			a.id IN (SELECT agent_id FROM grimm_managers WHERE session_id = $2 AND agent_id <> '')
			OR a.id IN (SELECT agent_id FROM grimm_workers WHERE session_id = $2 AND agent_id <> '')
		)`, host, sessionID)
	if err != nil {
		return "", "", err
	}
	cwds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", "", err
	}
	for _, cwd := range cwds {
		roots = appendSessionRoot(roots, cwd)
	}

	if len(roots) == 0 || !pathWithinAnyRoot(cleanPath, roots) {
		return "", "", &grimm.Error{Status: http.StatusForbidden, Msg: "file is outside this session's workspace"}
	}
	if !online {
		return "", "", &grimm.Error{Status: http.StatusServiceUnavailable, Msg: fmt.Sprintf("host %s is offline", host)}
	}

	args, err := json.Marshal(hostclient.ReadFileArgs{Path: cleanPath, Roots: roots})
	if err != nil {
		return "", "", err
	}
	cmd, err := a.reg.Enqueue(ctx, host, hostclient.ReadFileKind, args, grimmFileReadTTL, a.cfg.Now())
	if err != nil {
		if errors.Is(err, fleet.ErrUnknownHost) {
			return "", "", &grimm.Error{Status: http.StatusServiceUnavailable, Msg: fmt.Sprintf("host %s has not connected", host)}
		}
		var bad *fleet.BadCommandError
		if errors.As(err, &bad) {
			return "", "", &grimm.Error{Status: http.StatusBadRequest, Msg: bad.Error()}
		}
		return "", "", err
	}
	return cmd.ID, cleanPath, nil
}

func cleanRequestedFilePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", errors.New("path is required")
	}
	if strings.ContainsRune(p, 0) || !path.IsAbs(p) || slices.Contains(strings.Split(p, "/"), "..") {
		return "", errors.New("path must be a clean absolute path")
	}
	cp := path.Clean(p)
	if cp == "/" {
		return "", errors.New("path must name a file")
	}
	return cp, nil
}

func collectSessionRoots(candidates ...string) []string {
	var roots []string
	for _, c := range candidates {
		roots = appendSessionRoot(roots, c)
	}
	return roots
}

func appendSessionRoot(roots []string, raw string) []string {
	r := strings.TrimSpace(raw)
	if r == "" || !path.IsAbs(r) || strings.ContainsRune(r, 0) || slices.Contains(strings.Split(r, "/"), "..") {
		return roots
	}
	cr := path.Clean(r)
	if cr == "/" || slices.Contains(roots, cr) {
		return roots
	}
	return append(roots, cr)
}

func pathWithinAnyRoot(cleanPath string, roots []string) bool {
	for _, r := range roots {
		if strings.HasPrefix(cleanPath, r+"/") {
			return true
		}
	}
	return false
}
