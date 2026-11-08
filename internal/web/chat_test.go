package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestTranscriptPageHasAChatBox(t *testing.T) {
	h, _ := cmdServer(t)
	page := get(t, h, "/hosts/host-1/agents/"+agentA1, nil).Body.String()
	for _, want := range []string{`id="chat"`, `data-commands="/api/hosts/host-1/commands"`, `data-agent="` + agentA1 + `"`,
		`data-behavior="steer"`, `data-behavior="queue"`, `data-behavior="interrupt"`, `name="text"`, `data-pending-sql="`} {
		if !strings.Contains(page, want) {
			t.Errorf("transcript page misses %s", want)
		}
	}
}

func TestChatMessagesGoThroughTheCommandsAPIAndReadBackThroughSQL(t *testing.T) {
	h, _ := cmdServer(t)
	for _, b := range []string{"steer", "interrupt", "queue"} {
		body := `{"kind":"send_message","args":{"agentId":"` + agentA1 + `","text":"` + b + ` me","behavior":"` + b + `"},"ttl":"24h"}`
		if code, _, msg := enqueue(t, h, "host-1", "op", body); code != http.StatusAccepted {
			t.Fatalf("%s: %d %s", b, code, msg)
		}
	}
	bad := `{"kind":"send_message","args":{"agentId":"` + agentA1 + `","text":"x","behavior":"whenever"}}`
	if code, _, _ := enqueue(t, h, "host-1", "op", bad); code != http.StatusBadRequest {
		t.Errorf("an unknown behavior: %d, want 400", code)
	}
	other := `{"kind":"send_message","args":{"agentId":"someone-else","text":"x"}}`
	if code, _, msg := enqueue(t, h, "host-1", "op", other); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, msg)
	}

	code, r := sqlPost(t, h, q(pendingMessagesSQL("host-1", agentA1)))
	if code != http.StatusOK {
		t.Fatalf("pending SQL: %d %s", code, r.Error)
	}
	var got []string
	for _, row := range r.Rows {
		got = append(got, row[1].(string)+":"+row[2].(string))
	}
	if strings.Join(got, " ") != "steer me:steer interrupt me:interrupt queue me:queue" {
		t.Errorf("pending = %v (oldest first, this agent's only)", got)
	}

	code, r = sqlPost(t, h, q(pendingMessagesSQL("host-1", "x' OR '1'='1")))
	if code != http.StatusOK || len(r.Rows) != 0 {
		t.Errorf("a quote in an agent id: %d %v %s", code, r.Rows, r.Error)
	}
}
