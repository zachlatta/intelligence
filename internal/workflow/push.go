package workflow

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/zachlatta/intelligence/internal/webpush"
)

type pendingPush struct {
	accountID int64
	msg       webpush.Message
}

type pushCollector struct {
	items []pendingPush
}

type pushCollectorKey struct{}

// CollectPushes attaches a transaction-scoped push collector to ctx and returns a flush
// function to call after the transaction commits. Calling flush sends every push recorded
// during the transaction via e.Push; if the transaction rolls back, simply do not call flush.
func (e *Engine) CollectPushes(ctx context.Context) (context.Context, func()) {
	if e.Push == nil {
		return ctx, func() {}
	}
	if _, ok := ctx.Value(pushCollectorKey{}).(*pushCollector); ok {
		return ctx, func() {} // outer transaction owns the flush
	}
	c := &pushCollector{}
	return context.WithValue(ctx, pushCollectorKey{}, c), func() {
		e.pushBestEffort(ctx, c.items...)
	}
}

func addPush(ctx context.Context, item pendingPush) {
	if c, ok := ctx.Value(pushCollectorKey{}).(*pushCollector); ok {
		c.items = append(c.items, item)
	}
}

func (e *Engine) pushBestEffort(ctx context.Context, items ...pendingPush) {
	if e.Push == nil || len(items) == 0 {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for _, item := range items {
		if item.accountID == 0 {
			continue
		}
		if _, _, err := e.Push.SendAccount(bounded, item.accountID, item.msg); err != nil && ctx.Err() == nil {
			log.Printf("web push (%s): %v", item.msg.Tag, err)
		}
	}
}

func runPushLabel(summary, title string) string {
	if s := strings.TrimSpace(summary); s != "" {
		return s
	}
	return strings.TrimSpace(title)
}

func cleanPushNote(note string) string {
	fields := strings.Fields(note)
	return strings.Join(fields, " ")
}

func askPush(accountID int64, runID, label, note string) pendingPush {
	title := "Needs your reply"
	if label != "" {
		title += ": " + label
	}
	return pendingPush{
		accountID: accountID,
		msg: webpush.Message{
			Title: clipRunes(title, 60),
			Body:  clipRunes(cleanPushNote(note), 200),
			URL:   "/workflow-runs/" + runID,
			Tag:   "run-ask-" + runID,
		},
	}
}

func failPush(accountID int64, runID, label, errMsg string) pendingPush {
	title := "Workflow run failed"
	if label != "" {
		title += ": " + label
	}
	return pendingPush{
		accountID: accountID,
		msg: webpush.Message{
			Title: clipRunes(title, 60),
			Body:  clipRunes(plainFailureError(errMsg), 200),
			URL:   "/workflow-runs/" + runID,
			Tag:   "run-failed-" + runID,
		},
	}
}
