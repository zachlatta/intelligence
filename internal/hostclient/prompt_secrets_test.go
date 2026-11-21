package hostclient

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCommandsRefuseCredentialsInAgentText(t *testing.T) {
	for _, prefix := range []string{"pdw_" + strings.Repeat("a", 12) + "_", "ibk_", "imcp_"} {
		secret := prefix + strings.Repeat("x", 43)
		for _, tc := range []struct {
			kind string
			args any
		}{
			{CreateAgentKind, CreateAgentArgs{Cwd: "/w", Provider: "claude", Prompt: secret}},
			{CreateWorkspaceKind, CreateWorkspaceArgs{Cwd: "/w", Agent: &CreateAgentArgs{Provider: "claude", Prompt: secret}}},
			{SendMessageKind, AgentArgs{AgentID: "a", Text: secret}},
			{SendMessageKind, AgentArgs{AgentID: "a", Text: secret, Behavior: Queue}},
			{SendMessageKind, AgentArgs{AgentID: "a", Text: secret, Behavior: Interrupt}},
			{CreateScheduleKind, map[string]any{"prompt": secret, "every": "1h", "agent": map[string]string{"provider": "claude", "cwd": "/w"}}},
			{UpdateScheduleKind, map[string]any{"scheduleId": "s", "prompt": secret}},
		} {
			raw, _ := json.Marshal(tc.args)
			_, err := ParseCommand(tc.kind, raw)
			if err == nil {
				t.Errorf("%s accepted credential prefix %s", tc.kind, prefix)
			} else if strings.Contains(err.Error(), secret) {
				t.Error("refusal leaked credential")
			}
		}
	}
}

func TestCommandCredentialChecksDecodeEscapesAndDoNotEchoFields(t *testing.T) {
	secret := "pdw_" + strings.Repeat("a", 12) + "_" + strings.Repeat("x", 40)
	for _, raw := range []string{
		`{"agentId":"a","text":"` + strings.ReplaceAll(secret, "p", `\u0070`) + `"}`,
		`{"` + secret + `":"value"} trailing`,
	} {
		_, err := ParseCommand(SendMessageKind, json.RawMessage(raw))
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Error("credential accepted or echoed in decoder error")
		}
	}
}
