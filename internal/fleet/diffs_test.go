package fleet

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/paseo"
)

func workspaceDiff(id string, additions int) hostclient.WorkspaceDiff {
	return hostclient.WorkspaceDiff{WorkspaceID: id, Directory: "/w/" + id, CollectedAt: t1, Additions: additions, Deletions: 1,
		Files: []paseo.DiffFile{{Path: "main.go", Additions: additions, Deletions: 1, Status: "ok", Hunks: []paseo.DiffHunk{
			{OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 1, Lines: []paseo.DiffLine{{Type: "remove", Content: "a"}, {Type: "add", Content: "b"}}}}}}}
}

// storedDiff is a workspace's stored diff: its file count, additions and files' paths; ok false for none.
func storedDiff(t *testing.T, r *Registry, host, ws string) (files, additions int, paths []string, errText string, ok bool) {
	t.Helper()
	var raw []byte
	err := r.p.QueryRow(ctx, `SELECT file_count, additions, error, files FROM workspace_diffs WHERE host = $1 AND workspace_id = $2`,
		host, ws).Scan(&files, &additions, &errText, &raw)
	if err != nil {
		return 0, 0, nil, "", false
	}
	var fs []paseo.DiffFile
	if err := json.Unmarshal(raw, &fs); err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		paths = append(paths, f.Path)
	}
	return files, additions, paths, errText, true
}

func TestRecordDiffKeepsEachWorkspacesLatest(t *testing.T) {
	r := New(dbtestOwner(t))
	if err := r.Record(ctx, full(), t1); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordDiff(ctx, "host-1", workspaceDiff("w1", 2), t1); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordDiff(ctx, "host-1", workspaceDiff("w1", 5), t1.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	files, adds, paths, _, ok := storedDiff(t, r, "host-1", "w1")
	if !ok || files != 1 || adds != 5 || len(paths) != 1 || paths[0] != "main.go" {
		t.Errorf("w1's diff: %d files, +%d, %v, %v", files, adds, paths, ok)
	}
	var lines int
	if err := r.p.QueryRow(ctx, `SELECT sum(jsonb_array_length(h -> 'lines'))::int FROM workspace_diff_files f,
		jsonb_array_elements(f.hunks) h WHERE host = 'host-1' AND workspace_id = 'w1' AND path = 'main.go'`).Scan(&lines); err != nil || lines != 2 {
		t.Errorf("workspace_diff_files: %d lines, %v", lines, err)
	}

	refused := hostclient.WorkspaceDiff{WorkspaceID: "w2", Directory: "/w/docs", CollectedAt: t1, Error: "Not a git repository: /w/docs"}
	if err := r.RecordDiff(ctx, "host-1", refused, t1); err != nil {
		t.Fatal(err)
	}
	if files, _, _, msg, ok := storedDiff(t, r, "host-1", "w2"); !ok || files != 0 || msg != refused.Error {
		t.Errorf("w2's diff: %d files, %q, %v", files, msg, ok)
	}
}

func TestADiffGoesWithItsWorkspace(t *testing.T) {
	r := New(dbtestOwner(t))
	rep := full()
	if err := r.Record(ctx, rep, t1); err != nil {
		t.Fatal(err)
	}
	_ = r.RecordDiff(ctx, "host-1", workspaceDiff("w1", 2), t1)
	_ = r.RecordDiff(ctx, "host-1", workspaceDiff("w3", 2), t1)

	// the daemon is gone: nothing says the workspaces are, so their diffs stay
	down := rep
	down.Paseo = hostclient.PaseoStatus{Error: "connecting to the Paseo daemon"}
	if err := r.Record(ctx, down, t1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, ok := storedDiff(t, r, "host-1", "w1"); !ok {
		t.Error("a diff went when the daemon did")
	}

	// w1 is archived
	rep.Paseo.Workspaces = rep.Paseo.Workspaces[1:]
	if err := r.Record(ctx, rep, t1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, ok := storedDiff(t, r, "host-1", "w1"); ok {
		t.Error("an archived workspace's diff is kept")
	}
	if _, _, _, _, ok := storedDiff(t, r, "host-1", "w3"); !ok {
		t.Error("a listed workspace's diff went")
	}
}
