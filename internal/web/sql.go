package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SQL API limits.
const (
	defaultSQLLimit = 1000
	maxSQLLimit     = 10000
	maxSQLBody      = 64 << 10
)

type sqlRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit"` // rows to return; default 1000, at most 10000
}

type sqlColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type sqlResponse struct {
	Columns   []sqlColumn `json:"columns"`
	Rows      [][]any     `json:"rows"`
	RowCount  int         `json:"rowCount"`
	Truncated bool        `json:"truncated"` // more rows matched than the limit
}

// sqlHandler runs one read-only query through p, the account's own role, which reads only the
// account's rows: POST {"query", "limit"} or GET ?q=&limit=.
func sqlHandler(cfg Config, p *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req sqlRequest
		if r.Method == http.MethodGet {
			req.Query = r.URL.Query().Get("q")
			req.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
		} else if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSQLBody)).Decode(&req); err != nil {
			sqlError(w, http.StatusBadRequest, "bad request: want JSON {\"query\": \"...\", \"limit\": n}: "+err.Error())
			return
		}
		if strings.TrimSpace(req.Query) == "" {
			sqlError(w, http.StatusBadRequest, "empty query")
			return
		}
		if req.Limit <= 0 {
			req.Limit = defaultSQLLimit
		}
		req.Limit = min(req.Limit, maxSQLLimit)
		res, err := runReadonly(r.Context(), cfg, p, req)
		if err != nil {
			sqlError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, res)
	}
}

func runReadonly(ctx context.Context, cfg Config, p *pgxpool.Pool, req sqlRequest) (sqlResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.SQLTimeout+2e9)
	defer cancel()
	tx, err := p.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return sqlResponse{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", cfg.SQLTimeout.Milliseconds())); err != nil {
		return sqlResponse{}, err
	}
	rows, err := tx.Query(ctx, req.Query)
	if err != nil {
		return sqlResponse{}, friendly(err)
	}
	defer rows.Close()
	res := sqlResponse{Columns: []sqlColumn{}, Rows: [][]any{}}
	tm := tx.Conn().TypeMap()
	for _, f := range rows.FieldDescriptions() {
		typ := strconv.Itoa(int(f.DataTypeOID))
		if t, ok := tm.TypeForOID(f.DataTypeOID); ok {
			typ = t.Name
		}
		res.Columns = append(res.Columns, sqlColumn{Name: f.Name, Type: typ})
	}
	for rows.Next() {
		if len(res.Rows) == req.Limit {
			res.Truncated = true
			break
		}
		vals, err := rows.Values()
		if err != nil {
			return sqlResponse{}, err
		}
		for i, v := range vals {
			vals[i] = jsonValue(v)
		}
		res.Rows = append(res.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return sqlResponse{}, friendly(err)
	}
	res.RowCount = len(res.Rows)
	return res, nil
}

// jsonValue makes database values read well as JSON: UUIDs as strings, JSON columns as JSON.
func jsonValue(v any) any {
	switch x := v.(type) {
	case [16]byte:
		return pgtype.UUID{Bytes: x, Valid: true}.String()
	case pgtype.Numeric:
		if f, err := x.Float64Value(); err == nil && f.Valid {
			return f.Float64
		}
	}
	return v
}

// friendly is a Postgres error with its code, and a plain word for a timeout.
func friendly(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		if pg.Code == "57014" {
			return fmt.Errorf("query cancelled by statement timeout: %s", pg.Message)
		}
		return fmt.Errorf("%s (SQLSTATE %s)", pg.Message, pg.Code)
	}
	return err
}

func sqlError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
