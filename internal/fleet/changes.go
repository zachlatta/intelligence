package fleet

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// channel is the Postgres NOTIFY channel the registry announces its changes on. A payload is a
// topic: what changed, for whoever waits on it.
const channel = "fleet"

// CommandsTopic: a command was queued for box.
func CommandsTopic(box string) string { return "commands:" + box }

// CommandTopic: the command with id changed (a box's update, or its expiry).
func CommandTopic(id string) string { return "command:" + id }

// StreamTopic: a stream attached for box, so any other stream it had is over.
func StreamTopic(box string) string { return "stream:" + box }

// Changes wakes the server's waiters when the fleet changes, whichever server instance changed
// it: the registry announces each change with a Postgres NOTIFY, and Run listens on one
// connection. Waking says only "look again": the state itself is in the tables.
type Changes struct {
	p     *pgxpool.Pool
	retry time.Duration

	mu    sync.Mutex
	subs  map[string]map[chan struct{}]struct{}
	ready chan struct{} // closed once Run first listens
	once  sync.Once
}

// NewChanges follows the changes announced in p's database.
func NewChanges(p *pgxpool.Pool) *Changes {
	return &Changes{p: p, retry: time.Second, subs: map[string]map[chan struct{}]struct{}{}, ready: make(chan struct{})}
}

// Subscribe wakes the returned channel (coalesced: one pending wake at most) after each change
// to topic, until stop. Subscribe before reading the state it guards, so no change between the
// read and the wait is missed.
func (c *Changes) Subscribe(topic string) (wake <-chan struct{}, stop func()) {
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	if c.subs[topic] == nil {
		c.subs[topic] = map[chan struct{}]struct{}{}
	}
	c.subs[topic][ch] = struct{}{}
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.subs[topic], ch)
		if len(c.subs[topic]) == 0 {
			delete(c.subs, topic)
		}
	}
}

// Ready waits until Run listens (or ctx ends): from then on no announced change is missed.
func (c *Changes) Ready(ctx context.Context) {
	select {
	case <-c.ready:
	case <-ctx.Done():
	}
}

// Run listens until ctx ends, connecting again when the connection drops. Every subscriber is
// woken each time it starts listening, as anything announced while it was not is lost.
func (c *Changes) Run(ctx context.Context) {
	lastErr := ""
	for ctx.Err() == nil {
		err := c.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if msg := err.Error(); msg != lastErr {
			log.Printf("listening for fleet changes: %v", err)
			lastErr = msg
		}
		select {
		case <-ctx.Done():
		case <-time.After(c.retry):
		}
	}
}

func (c *Changes) listen(ctx context.Context) error {
	pc, err := c.p.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pc.Hijack() // held for good, so it leaves the pool
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	c.once.Do(func() { close(c.ready) })
	c.wakeAll()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		c.wake(n.Payload)
	}
}

func (c *Changes) wake(topic string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ch := range c.subs[topic] {
		signal(ch)
	}
}

func (c *Changes) wakeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, subs := range c.subs {
		for ch := range subs {
			signal(ch)
		}
	}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// notifySQL announces a topic; run it in the transaction that makes the change, so the wake
// comes after the change commits and never without it.
const notifySQL = "SELECT pg_notify('" + channel + "', $1)"

// NotifySQL announces a topic ($1), for changes made outside the registry.
const NotifySQL = notifySQL

// EnrollmentTopic: the dev box enrollment with id was claimed.
func EnrollmentTopic(id string) string { return "enrollment:" + id }
