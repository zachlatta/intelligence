package web

import "strings"

// The transcript page's chat box sends the agent messages through the commands API (send_message,
// with a behavior: steer, interrupt or queue) and reads back the ones not yet delivered through
// the read-only SQL API, with pendingMessagesSQL.

// queuedMessageTTL is how long a queued message may wait for the agent's turn to end; other
// messages take the commands API's default.
const queuedMessageTTL = "24h"

// pendingMessagesSQL reads the messages to box's agent that have not gone yet, oldest first: id,
// text, behavior, status and when each was sent.
func pendingMessagesSQL(box, agentID string) string {
	return `SELECT id::text, args ->> 'text' AS text, coalesce(nullif(args ->> 'behavior', ''), 'steer') AS behavior, status,
	created_at FROM commands WHERE box = ` + sqlString(box) + ` AND kind = 'send_message' AND args ->> 'agentId' = ` +
		sqlString(agentID) + ` AND status IN ('queued', 'sent', 'running') ORDER BY created_at, id`
}

// sqlString quotes s as a Postgres string literal (standard_conforming_strings: only ' is special).
func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
