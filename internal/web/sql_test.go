package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
)

type sqlResult struct {
	Columns []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"columns"`
	Rows      [][]any `json:"rows"`
	RowCount  int     `json:"rowCount"`
	Truncated bool    `json:"truncated"`
	Error     string  `json:"error"`
}

func sqlPost(t *testing.T, h http.Handler, body string) (int, sqlResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sql", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r sqlResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, r
}

func q(query string) string {
	b, _ := json.Marshal(map[string]string{"query": query})
	return string(b)
}

func TestSQLReadsAppState(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, sampleReport)
	code, r := sqlPost(t, h, q("SELECT name, paseo_version, online, running_agents::text AS running FROM devbox_status"))
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, r.Error)
	}
	if len(r.Columns) != 4 || r.Columns[0].Name != "name" || r.Columns[2].Type != "bool" {
		t.Errorf("columns = %+v", r.Columns)
	}
	if r.RowCount != 1 || len(r.Rows) != 1 || r.Rows[0][0] != "devbox-1" || r.Rows[0][1] != "0.15.3" || r.Rows[0][2] != true || r.Rows[0][3] != "1" {
		t.Errorf("rows = %+v", r.Rows)
	}
}

func TestSQLGETWithQueryParameter(t *testing.T) {
	h, _ := dbServer(t)
	rec := get(t, h, "/api/sql?q="+url.QueryEscape("SELECT 1 + 1 AS two"), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"two"`) || !strings.Contains(rec.Body.String(), "[[2]]") {
		t.Fatalf("GET /api/sql = %d %s", rec.Code, rec.Body)
	}
}

func TestSQLRefusesWrites(t *testing.T) {
	h, _ := dbServer(t)
	for _, query := range []string{
		"INSERT INTO devboxes (name, last_seen) VALUES ('x', now())",
		"UPDATE devboxes SET daemon = 'x'",
		"DELETE FROM schema_migrations",
		"CREATE TABLE sneaky (x int)",
		"DROP TABLE devboxes",
		"SELECT set_config('role', 'postgres', false)",
		"SELECT 1; DELETE FROM devboxes",
	} {
		if code, r := sqlPost(t, h, q(query)); code != http.StatusBadRequest || r.Error == "" {
			t.Errorf("%q: status %d error %q, want 400 with the database's error", query, code, r.Error)
		}
	}
}

func TestSQLCapsRows(t *testing.T) {
	h, _ := dbServer(t)
	code, r := sqlPost(t, h, `{"query": "SELECT generate_series(1, 50)", "limit": 10}`)
	if code != http.StatusOK || r.RowCount != 10 || !r.Truncated {
		t.Fatalf("status %d rowCount %d truncated %v", code, r.RowCount, r.Truncated)
	}
	if _, r := sqlPost(t, h, `{"query": "SELECT generate_series(1, 5)", "limit": 10}`); r.Truncated || r.RowCount != 5 {
		t.Errorf("5 rows under a limit of 10: rowCount %d truncated %v", r.RowCount, r.Truncated)
	}
}

func TestSQLTimesOut(t *testing.T) {
	d := dbtest.New(t)
	h := New(Config{Context: t.Context(), Version: "v", DB: d.Owner, Readonly: d.Readonly, DevboxToken: "secret", SQLTimeout: 200 * time.Millisecond})
	start := time.Now()
	code, r := sqlPost(t, h, q("SELECT pg_sleep(5)"))
	if code != http.StatusBadRequest || !strings.Contains(r.Error, "timeout") || time.Since(start) > 3*time.Second {
		t.Fatalf("status %d error %q after %v", code, r.Error, time.Since(start))
	}
}

func TestSQLRejectsEmptyQuery(t *testing.T) {
	h, _ := dbServer(t)
	if code, _ := sqlPost(t, h, `{"query": "  "}`); code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", code)
	}
}

// The contract, through the API: every table and view in the app schema can be read with SQL.
func TestEveryTableIsReadableThroughTheSQLAPI(t *testing.T) {
	d := dbtest.New(t)
	h := New(Config{Context: t.Context(), Version: "v", DB: d.Owner, Readonly: d.Readonly, DevboxToken: "secret"})
	rows, err := d.Owner.Query(t.Context(), `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	if len(tables) < 3 {
		t.Fatalf("tables = %v, want devboxes, devbox_status and schema_migrations at least", tables)
	}
	for _, tbl := range tables {
		if code, r := sqlPost(t, h, q(`SELECT * FROM "`+tbl+`" LIMIT 1`)); code != http.StatusOK {
			t.Errorf("%s: status %d %s", tbl, code, r.Error)
		}
	}
}

func TestSQLWalksDevboxesWorkspacesAndAgents(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, sampleReport)
	code, r := sqlPost(t, h, q(`SELECT d.name, w.name, a.title, a.status FROM devboxes d JOIN workspaces w ON w.box = d.name
		JOIN agents a ON a.box = w.box AND a.workspace_id = w.id ORDER BY a.id`))
	if code != http.StatusOK || r.RowCount != 3 || r.Rows[0][0] != "devbox-1" || r.Rows[0][1] != "Login fix" || r.Rows[0][3] != "running" {
		t.Fatalf("status %d rows %+v error %q", code, r.Rows, r.Error)
	}
}
