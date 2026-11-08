package fleet

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// changes runs a Changes on p until the test ends.
func changes(t *testing.T, p *pgxpool.Pool) *Changes {
	t.Helper()
	c := NewChanges(p)
	go c.Run(t.Context())
	return c
}

func woken(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: never woken", what)
	}
}

func notWoken(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s: woken", what)
	case <-time.After(200 * time.Millisecond):
	}
}

// drain empties a wake channel, so the next receive is a new wake.
func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func TestChangesWakeTheirTopicsSubscribersOnEveryInstance(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	record(t, r, full(), t1)
	here, there := changes(t, d.Owner), changes(t, dbtest.Pool(t, d))
	queued, stop := here.Subscribe(CommandsTopic("host-1"))
	defer stop()
	elsewhere, stop2 := there.Subscribe(CommandsTopic("host-1"))
	defer stop2()
	other, stop3 := there.Subscribe(CommandsTopic("host-2"))
	defer stop3()
	here.Ready(t.Context())
	there.Ready(t.Context())
	drain(queued)
	drain(elsewhere)
	drain(other)

	c := enqueue(t, r, hostclient.ArchiveAgentKind, `{"agentId":"a1"}`, t1)
	woken(t, queued, "this instance")
	woken(t, elsewhere, "another instance")
	notWoken(t, other, "another host's subscriber")

	done, stop4 := there.Subscribe(CommandTopic(c.ID))
	defer stop4()
	drain(done)
	if err := r.UpdateCommand(ctx, "host-1", hostclient.CommandUpdate{ID: c.ID, Status: hostclient.CommandSucceeded}, t1); err != nil {
		t.Fatal(err)
	}
	woken(t, done, "a command's waiter")
}

func TestChangesWakeEveryoneWhenTheyStartListening(t *testing.T) {
	d := dbtest.New(t)
	c := NewChanges(d.Owner)
	wake, stop := c.Subscribe(CommandsTopic("host-1"))
	defer stop()
	go c.Run(t.Context()) // anything announced before it listened is lost, so its subscribers look again
	woken(t, wake, "a subscriber from before Run")
}

func TestAStreamOwnsItsHostUntilAnotherAttaches(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	ch := changes(t, d.Owner)
	now := time.Now()
	attached, stop := ch.Subscribe(StreamTopic("host-1"))
	defer stop()
	ch.Ready(t.Context())
	drain(attached)

	// a host that has never reported can attach: its row is made, and it is online
	if err := r.Attach(ctx, "host-1", "11111111-1111-4111-8111-111111111111", now); err != nil {
		t.Fatal(err)
	}
	woken(t, attached, "attach")
	if !online(t, d, "host-1") {
		t.Errorf("a host with a stream is online")
	}
	record(t, r, full(), now) // a report keeps the stream
	if cur, err := r.Seen(ctx, "host-1", "11111111-1111-4111-8111-111111111111", now); err != nil || !cur {
		t.Fatalf("seen after a report: %v, %v", cur, err)
	}

	drain(attached)
	if err := r.Attach(ctx, "host-1", "22222222-2222-4222-8222-222222222222", now); err != nil {
		t.Fatal(err)
	}
	woken(t, attached, "a second attach")
	if cur, err := r.Seen(ctx, "host-1", "11111111-1111-4111-8111-111111111111", now); err != nil || cur {
		t.Fatalf("the replaced stream is still current: %v, %v", cur, err)
	}
	if err := r.Detach(ctx, "host-1", "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
	if !online(t, d, "host-1") {
		t.Errorf("the replaced stream ending took the host offline")
	}
	if err := r.Detach(ctx, "host-1", "22222222-2222-4222-8222-222222222222"); err != nil {
		t.Fatal(err)
	}
	if online(t, d, "host-1") {
		t.Errorf("a host with no stream is online")
	}
}

func TestAStreamNotSeenForAMinuteIsOffline(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	old := time.Now().Add(-2 * time.Minute) // a server that died with the stream open
	if err := r.Attach(ctx, "host-1", "11111111-1111-4111-8111-111111111111", old); err != nil {
		t.Fatal(err)
	}
	if online(t, d, "host-1") {
		t.Errorf("online with nothing heard for 2 minutes")
	}
	if _, err := r.Seen(ctx, "host-1", "11111111-1111-4111-8111-111111111111", time.Now()); err != nil {
		t.Fatal(err)
	}
	if !online(t, d, "host-1") {
		t.Errorf("offline right after being seen")
	}
}

func online(t *testing.T, d dbtest.DB, host string) bool {
	t.Helper()
	var on bool
	if err := dbtest.Reader(t, d, host).QueryRow(ctx, "SELECT online FROM host_status WHERE name = $1", host).Scan(&on); err != nil {
		t.Fatal(err)
	}
	return on
}
