package paseo_test

import (
	"testing"

	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

func TestDenyPermissionUsesPromptID(t *testing.T) {
	d := paseotest.New(t, token)
	c := dial(t, d)
	if err := c.DenyPermission(ctx5(t), "a", "permission-1", "rewrite"); err != nil {
		t.Fatal(err)
	}
	req := d.Requests("agent_permission_response")[0]
	response := req["response"].(map[string]any)
	if req["requestId"] != "permission-1" || req["agentId"] != "a" || response["behavior"] != "deny" || response["message"] != "rewrite" {
		t.Fatal(req)
	}
}
