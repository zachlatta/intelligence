// Package fleet keeps the latest report from each dev box. It is in memory: after a server restart,
// each box reappears with its next report.
package fleet

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zachlatta/intelligence/internal/devbox"
)

// Box is a dev box as the server last heard from it.
type Box struct {
	Report   devbox.Report `json:"report"`
	LastSeen time.Time     `json:"lastSeen"` // when the server received the report
	Online   bool          `json:"online"`   // it reported within the stale window
}

// Registry holds the latest report per box.
type Registry struct {
	now   func() time.Time
	stale time.Duration

	mu    sync.Mutex
	boxes map[string]Box
}

// New returns an empty registry. A box that has not reported for stale counts as offline.
func New(now func() time.Time, stale time.Duration) *Registry {
	return &Registry{now: now, stale: stale, boxes: map[string]Box{}}
}

// Record stores r as its box's latest report.
func (r *Registry) Record(rep devbox.Report) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.boxes[rep.Name] = Box{Report: rep, LastSeen: r.now()}
}

// List returns every box, by name, with its online state as of now.
func (r *Registry) List() []Box {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make([]Box, 0, len(r.boxes))
	for _, b := range r.boxes {
		b.Online = now.Sub(b.LastSeen) <= r.stale
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b Box) int { return strings.Compare(a.Report.Name, b.Report.Name) })
	return out
}
