//go:build !linux

package probe

import "context"

// Probe is the loaded probe; only Linux has one.
type Probe struct{}

// Start fails: the probe needs Linux.
func Start(context.Context, string) (*Probe, error) { return nil, ErrUnsupported }

// Snapshot is empty off Linux.
func (*Probe) Snapshot(uint64) Snapshot { return Snapshot{} }

// Close does nothing off Linux.
func (*Probe) Close() {}
