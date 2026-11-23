package workflow

import (
	"context"
	"sync"
	"testing"

	"github.com/zachlatta/intelligence/internal/webpush"
)

type spyPushSender struct {
	mu   sync.Mutex
	msgs []webpush.Message
}

func (s *spyPushSender) SendAccount(_ context.Context, _ int64, msg webpush.Message) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
	return 1, 0, nil
}

func TestCollectPushesNestedSavepointRollbackDiscardsPhantomPush(t *testing.T) {
	spy := &spyPushSender{}
	e := &Engine{Push: spy}

	outerCtx, flushOuter := e.CollectPushes(context.Background())

	// Savepoint 1: queues a push, then rolls back (commitSP1 is NOT called).
	sp1Ctx, _ := e.CollectPushes(outerCtx)
	addPush(sp1Ctx, failPush(1, "run-rolled-back", "Ghost", "rolled back"))

	// Savepoint 2: queues a push and commits into outer.
	sp2Ctx, commitSP2 := e.CollectPushes(outerCtx)
	addPush(sp2Ctx, failPush(1, "run-committed", "Real", "provider disabled"))
	commitSP2()

	// Nothing sent before outer commit.
	if len(spy.msgs) != 0 {
		t.Fatalf("sent before outer commit: %+v", spy.msgs)
	}

	flushOuter()
	if len(spy.msgs) != 1 || spy.msgs[0].Tag != "run-failed-run-committed" {
		t.Fatalf("sent = %+v, want only run-failed-run-committed", spy.msgs)
	}
}
