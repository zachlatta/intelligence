package probe

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

const markVictimFormat = `name: mark_victim
ID: 520
format:
	field:unsigned short common_type;	offset:0;	size:2;	signed:0;
	field:int common_pid;	offset:4;	size:4;	signed:1;

	field:int pid;	offset:8;	size:4;	signed:1;
	field:__data_loc char[] comm;	offset:12;	size:4;	signed:0;
	field:unsigned long total_vm;	offset:16;	size:8;	signed:0;
	field:unsigned long anon_rss;	offset:24;	size:8;	signed:0;
	field:unsigned long file_rss;	offset:32;	size:8;	signed:0;
	field:unsigned long shmem_rss;	offset:40;	size:8;	signed:0;

print fmt: "pid=%d"
`

func TestParseFormatFindsFieldOffsets(t *testing.T) {
	f := parseFormat(markVictimFormat)
	want := map[string]field{"pid": {8, 4}, "comm": {12, 4}, "anon_rss": {24, 8}, "file_rss": {32, 8}, "shmem_rss": {40, 8}, "common_pid": {4, 4}}
	for name, w := range want {
		if f[name] != w {
			t.Errorf("%s = %+v, want %+v", name, f[name], w)
		}
	}
}

func TestSocketServesSnapshots(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "probe.sock")
	src := fakeSource{snap: Snapshot{Epoch: "e1", Agents: map[string]Counters{"a1": {CPUNanos: 5e9, Execs: 3}},
		OOMs: []OOM{{Seq: 1, PID: 9, Comm: "node", AgentID: "a1"}, {Seq: 2, PID: 10}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan error, 1)
	go func() { ready <- Serve(ctx, sock, -1, src, func() { ready <- nil }) }()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	got, err := Fetch(ctx, sock, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Epoch != "e1" || got.Agents["a1"].Execs != 3 || len(got.OOMs) != 1 || got.OOMs[0].Seq != 2 {
		t.Errorf("snapshot = %+v", got)
	}
	if _, err := Fetch(ctx, filepath.Join(t.TempDir(), "none.sock"), 0); err == nil {
		t.Error("fetched from no probe")
	}
	_ = time.Second
}

type fakeSource struct{ snap Snapshot }

func (f fakeSource) Snapshot(after uint64) Snapshot {
	s := f.snap
	s.OOMs = nil
	for _, o := range f.snap.OOMs {
		if o.Seq > after {
			s.OOMs = append(s.OOMs, o)
		}
	}
	return s
}
