package web

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/fleet"
)

// The transcript's agent list re-reads itself through the SQL API. Like Hosts, it lists each
// sub-agent right under its parent, even when the sub-agent sorts first on its own.
func TestAgentPanelListsSubAgentsUnderTheirParent(t *testing.T) {
	h, _ := dbServer(t)
	rep := sampleReport
	rep.Paseo.Agents = slices.Clone(sampleReport.Paseo.Agents)
	rep.Paseo.Agents[0].ParentID = rep.Paseo.Agents[2].ID // the running a1 is a sub-agent of the older, idle a3
	sendReport(t, h, rep)
	var ws fleet.WorkspaceAgents
	ws.ID = "w-app"
	code, r := sqlPost(t, h, q(panelsFor("host-1", &ws).AgentsSQL))
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, r.Error)
	}
	var order []string
	for _, row := range r.Rows {
		order = append(order, row[0].(string)[:2])
	}
	if got := strings.Join(order, ","); got != "a3,a1" {
		t.Errorf("order = %v, want a3,a1", got)
	}
	if r.Columns[4].Name != "parent_id" {
		t.Errorf("columns = %+v; the page reads parent_id fifth", r.Columns)
	}
}
