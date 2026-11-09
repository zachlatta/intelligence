package paseo

import (
	"context"
	"errors"
)

// DenyPermission uses the permission's own request ID: the daemon correlates its
// agent_permission_resolved acknowledgement by this ID, not a fresh RPC ID.
func (c *Client) DenyPermission(ctx context.Context, agent, id, message string) error {
	var p struct {
		AgentID   string `json:"agentId"`
		RequestID string `json:"requestId"`
	}
	if err := c.command(ctx, map[string]any{"type": "agent_permission_response", "agentId": agent, "requestId": id,
		"response": map[string]any{"behavior": "deny", "message": message}}, &p); err != nil {
		return err
	}
	if p.AgentID != agent || p.RequestID != id {
		return errors.New("permission acknowledgement did not match")
	}
	return nil
}
