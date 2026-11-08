package devbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
)

// Queued messages. Paseo has no queue of its own (its app holds queued messages in the client), so
// the box holds them: a queued message is a send_message command that runs until its agent is not
// running and every message queued for that agent before it has gone, then steers (which, on an
// agent that is not running, starts a turn). The command shows as running in the meantime, and a
// redelivery after a restart waits again; the command ID is the message's idempotency key, so it
// is never sent twice.

// settleAfterSend is how long a queued message waits for word of the turn the one before it started,
// before trusting an agent's status again. The daemon answers a message once its turn has started,
// but the update saying so can arrive after the answer.
const settleAfterSend = 10 * time.Second

type queuedMessage struct {
	id string
	at time.Time
}

// busy: the agent is in a turn (or starting).
func busy(status string) bool { return status == "running" || status == "initializing" }

// wakeLocked wakes every queued message to look again. w.mu is held.
func (w *Watcher) wakeLocked() {
	if w.wake != nil {
		close(w.wake)
	}
	w.wake = make(chan struct{})
}

func (w *Watcher) sendQueued(ctx context.Context, cmd Command, a AgentArgs) error {
	w.mu.Lock()
	if w.queues == nil {
		w.queues, w.settling = map[string][]queuedMessage{}, map[string]time.Time{}
	}
	q := w.queues[a.AgentID]
	if !slices.ContainsFunc(q, func(m queuedMessage) bool { return m.id == cmd.ID }) {
		q = append(q, queuedMessage{cmd.ID, cmd.CreatedAt})
		slices.SortStableFunc(q, func(x, y queuedMessage) int { return cmp.Or(x.at.Compare(y.at), cmp.Compare(x.id, y.id)) })
		w.queues[a.AgentID] = q
	}
	if w.wake == nil {
		w.wakeLocked()
	}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.queues[a.AgentID] = slices.DeleteFunc(w.queues[a.AgentID], func(m queuedMessage) bool { return m.id == cmd.ID })
		if len(w.queues[a.AgentID]) == 0 {
			delete(w.queues, a.AgentID)
		}
		w.wakeLocked()
		w.mu.Unlock()
	}()
	for {
		w.mu.Lock()
		c, wake := w.client, w.wake
		ag, ok := w.agents[a.AgentID]
		first := w.queues[a.AgentID][0].id == cmd.ID
		settle := time.Until(w.settling[a.AgentID])
		w.mu.Unlock()
		if c != nil && !ok {
			return fmt.Errorf("agent %s is not on this box (archived?)", a.AgentID)
		}
		if c != nil && first && settle <= 0 && !busy(ag.Status) {
			if err := c.SendMessage(ctx, a.AgentID, a.Text, cmd.ID, paseo.Steer); err != nil {
				return err
			}
			w.mu.Lock()
			w.settling[a.AgentID] = time.Now().Add(settleAfterSend)
			w.mu.Unlock()
			return nil
		}
		var timer <-chan time.Time
		if settle > 0 {
			timer = time.After(settle)
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return errors.New("expired while waiting for the agent's turn to end")
			}
			return ctx.Err()
		case <-wake:
		case <-timer:
		}
	}
}
