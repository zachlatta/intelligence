package web

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runsPerPage is how many runs /workflows lists a page.
const runsPerPage = 50

// runsPage is a page of the runs list on /workflows: the runs matching a search (all of them
// without one), newest first.
type runsPage struct {
	Runs        []runRow
	Query       string // the search, as typed
	Page, Total int    // Page counts from 1; Total is how many runs match
}

func (p runsPage) First() int { return (p.Page-1)*runsPerPage + 1 }
func (p runsPage) Last() int  { return p.First() + len(p.Runs) - 1 }
func (p runsPage) Pages() int { return max(1, (p.Total+runsPerPage-1)/runsPerPage) }

// Newer and Older link to the pages either side, keeping the search; "" when there is none.
func (p runsPage) Newer() string {
	if p.Page <= 1 {
		return ""
	}
	return p.link(min(p.Page-1, p.Pages()))
}

func (p runsPage) Older() string {
	if p.Page >= p.Pages() {
		return ""
	}
	return p.link(p.Page + 1)
}

func (p runsPage) link(page int) string {
	v := url.Values{}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if p.Query != "" {
		v.Set("q", p.Query)
	}
	if len(v) == 0 {
		return "/workflows"
	}
	return "/workflows?" + v.Encode()
}

// runSearchText is what a search reads of a run: its summary and title, prompt, workflow, status,
// host, project, branch (which holds the run's id) and workspace.
const runSearchText = `concat_ws(' ', r.task_summary, r.title, r.input, w.name, r.status, r.host, r.repo, r.branch, r.workspace_title)`

// likeEscape makes s match itself in LIKE: "%" and "_" are not wildcards.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// readRunsPage reads one page of the account's runs, through its role, so the page shows what
// the same SQL would. A search matches runs that hold every word of it, in any case, anywhere in
// runSearchText.
func readRunsPage(ctx context.Context, p *pgxpool.Pool, query, page string) (runsPage, error) {
	out := runsPage{Query: strings.TrimSpace(query), Page: 1}
	if n, err := strconv.Atoi(page); err == nil && n > 1 && n <= 1_000_000 {
		out.Page = n
	}
	var words []string
	for _, w := range strings.Fields(out.Query) {
		words = append(words, "%"+likeEscape(w)+"%")
	}
	if len(words) > 20 {
		words = words[:20]
	}
	where := `WHERE (SELECT bool_and(` + runSearchText + ` ILIKE pat) FROM unnest($1::text[]) pat) IS NOT FALSE `
	if err := p.QueryRow(ctx, `SELECT count(*) FROM workflow_runs r JOIN workflows w ON w.id = r.workflow_id `+where, words).Scan(&out.Total); err != nil {
		return out, err
	}
	rows, err := p.Query(ctx, runRowSelect+where+"ORDER BY r.created_at DESC, r.id DESC LIMIT $2 OFFSET $3",
		words, runsPerPage, int64(out.Page-1)*runsPerPage)
	if err != nil {
		return out, err
	}
	out.Runs, err = pgx.CollectRows(rows, scanRunRow)
	return out, err
}
