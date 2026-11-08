//go:build linux

package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

const (
	maxAgents = 4096    // agent slots; when they run out the probe starts a new epoch
	maxTasks  = 1 << 18 // processes and threads followed
	eventSize = 56      // event record: see the layout in events()
	evExec    = 1
	evOOM     = 2
	tracefs   = "/sys/kernel/tracing/events"
	oomKeep   = 1000 // OOM kills kept for the client
)

// Probe is the loaded probe.
type Probe struct {
	Version string
	proc    string

	tags, cpu, execs, events *ebpf.Map
	links                    []link.Link
	progs                    []*ebpf.Program
	rd                       *ringbuf.Reader

	mu    sync.Mutex
	epoch string
	ids   []string          // agent slot -> agent id
	slot  map[string]uint32 // agent id -> slot
	ooms  []OOM
	seq   uint64
}

// Start loads the probe's programs and attaches them: it needs root (CAP_BPF and CAP_PERFMON).
// It runs until ctx ends.
func Start(ctx context.Context, version string) (*Probe, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("memlock: %w", err)
	}
	p := &Probe{Version: version, proc: "/proc", slot: map[string]uint32{}}
	p.newEpoch()
	var err error
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	if p.tags, err = ebpf.NewMap(&ebpf.MapSpec{Name: "tags", Type: ebpf.LRUHash, KeySize: 4, ValueSize: 4, MaxEntries: maxTasks}); err != nil {
		return nil, fmt.Errorf("tags map: %w", err)
	}
	if p.cpu, err = ebpf.NewMap(&ebpf.MapSpec{Name: "cpu", Type: ebpf.PerCPUArray, KeySize: 4, ValueSize: 8, MaxEntries: maxAgents}); err != nil {
		return nil, fmt.Errorf("cpu map: %w", err)
	}
	if p.execs, err = ebpf.NewMap(&ebpf.MapSpec{Name: "execs", Type: ebpf.PerCPUArray, KeySize: 4, ValueSize: 8, MaxEntries: maxAgents}); err != nil {
		return nil, fmt.Errorf("execs map: %w", err)
	}
	last, err := ebpf.NewMap(&ebpf.MapSpec{Name: "last_switch", Type: ebpf.PerCPUArray, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		return nil, fmt.Errorf("switch map: %w", err)
	}
	defer last.Close() // the program holds it
	if p.events, err = ebpf.NewMap(&ebpf.MapSpec{Name: "events", Type: ebpf.RingBuf, MaxEntries: 1 << 18}); err != nil {
		return nil, fmt.Errorf("events map: %w", err)
	}
	forkF, err := readFormat("sched", "sched_process_fork")
	if err != nil {
		return nil, err
	}
	execF, err := readFormat("sched", "sched_process_exec")
	if err != nil {
		return nil, err
	}
	oomF, err := readFormat("oom", "mark_victim")
	if err != nil {
		return nil, err
	}
	progs := []struct {
		group, name string
		insns       asm.Instructions
		err         error
	}{
		{group: "sched", name: "sched_switch"},
		{group: "sched", name: "sched_process_fork"},
		{group: "sched", name: "sched_process_exec"},
		{group: "oom", name: "mark_victim"},
	}
	progs[0].insns = switchProg(last.FD(), p.tags.FD(), p.cpu.FD())
	progs[1].insns, progs[1].err = forkProg(forkF, p.tags.FD())
	progs[2].insns, progs[2].err = execProg(execF, p.tags.FD(), p.execs.FD(), p.events.FD())
	progs[3].insns, progs[3].err = oomProg(oomF, p.tags.FD(), p.events.FD())
	for _, pr := range progs {
		if pr.err != nil {
			err = fmt.Errorf("%s: %w", pr.name, pr.err)
			return nil, err
		}
		var prog *ebpf.Program
		prog, err = ebpf.NewProgram(&ebpf.ProgramSpec{Name: shortName(pr.name), Type: ebpf.TracePoint, License: "GPL", Instructions: pr.insns})
		if err != nil {
			err = fmt.Errorf("load %s: %w", pr.name, err)
			return nil, err
		}
		p.progs = append(p.progs, prog)
		var l link.Link
		if l, err = link.Tracepoint(pr.group, pr.name, prog, nil); err != nil {
			err = fmt.Errorf("attach %s: %w", pr.name, err)
			return nil, err
		}
		p.links = append(p.links, l)
	}
	if p.rd, err = ringbuf.NewReader(p.events); err != nil {
		return nil, fmt.Errorf("events reader: %w", err)
	}
	p.scan()
	go p.readEvents()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				p.Close()
				return
			case <-t.C:
				p.scan()
			}
		}
	}()
	return p, nil
}

func shortName(s string) string {
	s = strings.TrimPrefix(s, "sched_")
	if len(s) > 15 {
		s = s[:15]
	}
	return s
}

// Close detaches and unloads everything.
func (p *Probe) Close() {
	if p.rd != nil {
		p.rd.Close()
	}
	for _, l := range p.links {
		l.Close()
	}
	for _, pr := range p.progs {
		pr.Close()
	}
	for _, m := range []*ebpf.Map{p.tags, p.cpu, p.execs, p.events} {
		if m != nil {
			m.Close()
		}
	}
}

func readFormat(group, name string) (map[string]field, error) {
	b, err := os.ReadFile(filepath.Join(tracefs, group, name, "format"))
	if err != nil {
		b, err = os.ReadFile(filepath.Join("/sys/kernel/debug/tracing/events", group, name, "format"))
	}
	if err != nil {
		return nil, fmt.Errorf("tracepoint %s:%s: %w", group, name, err)
	}
	return parseFormat(string(b)), nil
}

func need(f map[string]field, name string, size int) (int16, error) {
	x, ok := f[name]
	if !ok || x.size != size {
		return 0, fmt.Errorf("no %d-byte field %q in the tracepoint", size, name)
	}
	return int16(x.offset), nil
}

// The programs. Each keeps keys on its stack: r10-4 and r10-8 (and -12) are 4-byte keys; an
// event is built at r10-72 (56 bytes):
//
//	0 type u32 | 4 pid u32 | 8 agent slot i32 (-1 none) | 12 pad | 16 anon kB | 24 file kB | 32 shmem kB | 40 unused [16]

// switchProg charges the time since this CPU's last switch to the process switched away from
// (the current task) when it belongs to an agent.
func switchProg(last, tags, cpu int) asm.Instructions {
	return asm.Instructions{
		asm.FnKtimeGetNs.Call(),
		asm.Mov.Reg(asm.R7, asm.R0),
		asm.StoreImm(asm.RFP, -4, 0, asm.Word),
		asm.LoadMapPtr(asm.R1, last),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -4),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		asm.LoadMem(asm.R8, asm.R0, 0, asm.DWord),
		asm.StoreMem(asm.R0, 0, asm.R7, asm.DWord),
		asm.JEq.Imm(asm.R8, 0, "exit"),
		asm.Sub.Reg(asm.R7, asm.R8), // r7 = ns on CPU
		asm.FnGetCurrentPidTgid.Call(),
		asm.RSh.Imm(asm.R0, 32),
		asm.StoreMem(asm.RFP, -8, asm.R0, asm.Word),
		asm.LoadMapPtr(asm.R1, tags),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -8),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		asm.LoadMem(asm.R1, asm.R0, 0, asm.Word),
		asm.StoreMem(asm.RFP, -12, asm.R1, asm.Word),
		asm.LoadMapPtr(asm.R1, cpu),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -12),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		asm.LoadMem(asm.R1, asm.R0, 0, asm.DWord),
		asm.Add.Reg(asm.R1, asm.R7),
		asm.StoreMem(asm.R0, 0, asm.R1, asm.DWord),
		asm.Mov.Imm(asm.R0, 0).WithSymbol("exit"),
		asm.Return(),
	}
}

// forkProg gives a new process or thread its parent's agent. When the parent has none it clears
// whatever an earlier process with the same (reused) pid left: exited processes are not removed
// at exit, since their last time on a CPU is charged after it, but age out of the LRU map.
func forkProg(f map[string]field, tags int) (asm.Instructions, error) {
	child, err := need(f, "child_pid", 4)
	if err != nil {
		return nil, err
	}
	return asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1),
		asm.LoadMem(asm.R1, asm.R6, child, asm.Word),
		asm.StoreMem(asm.RFP, -12, asm.R1, asm.Word),
		asm.FnGetCurrentPidTgid.Call(),
		asm.RSh.Imm(asm.R0, 32),
		asm.StoreMem(asm.RFP, -4, asm.R0, asm.Word),
		asm.LoadMapPtr(asm.R1, tags),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -4),
		asm.FnMapLookupElem.Call(),
		asm.JNE.Imm(asm.R0, 0, "inherit"),
		asm.LoadMapPtr(asm.R1, tags),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -12),
		asm.FnMapDeleteElem.Call(),
		asm.Ja.Label("exit"),
		asm.LoadMem(asm.R1, asm.R0, 0, asm.Word).WithSymbol("inherit"),
		asm.StoreMem(asm.RFP, -8, asm.R1, asm.Word),
		asm.LoadMapPtr(asm.R1, tags),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -12),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, -8),
		asm.Mov.Imm(asm.R4, 0), // BPF_ANY
		asm.FnMapUpdateElem.Call(),
		asm.Mov.Imm(asm.R0, 0).WithSymbol("exit"),
		asm.Return(),
	}, nil
}

// zeroEvent clears the event's 56 bytes (8-byte immediate stores do not exist: it uses r1).
func zeroEvent() asm.Instructions {
	out := asm.Instructions{asm.Mov.Imm(asm.R1, 0)}
	for off := int16(-72); off < -16; off += 8 {
		out = append(out, asm.StoreMem(asm.RFP, off, asm.R1, asm.DWord))
	}
	return out
}

// execProg counts a program started by an agent's process; one started by a process with no
// agent is sent to user space, which reads its environment.
func execProg(f map[string]field, tags, execs, events int) (asm.Instructions, error) {
	pid, err := need(f, "pid", 4)
	if err != nil {
		return nil, err
	}
	insns := asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1),
		asm.LoadMem(asm.R1, asm.R6, pid, asm.Word),
		asm.StoreMem(asm.RFP, -4, asm.R1, asm.Word),
		asm.LoadMapPtr(asm.R1, tags),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -4),
		asm.FnMapLookupElem.Call(),
		asm.JNE.Imm(asm.R0, 0, "tagged"),
	}
	insns = append(insns, zeroEvent()...)
	insns = append(insns,
		asm.StoreImm(asm.RFP, -72, evExec, asm.Word),
		asm.LoadMem(asm.R1, asm.RFP, -4, asm.Word),
		asm.StoreMem(asm.RFP, -68, asm.R1, asm.Word),
		asm.StoreImm(asm.RFP, -64, -1, asm.Word),
		asm.LoadMapPtr(asm.R1, events),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -72),
		asm.Mov.Imm(asm.R3, eventSize),
		asm.Mov.Imm(asm.R4, 0),
		asm.FnRingbufOutput.Call(),
		asm.Ja.Label("exit"),
		asm.LoadMem(asm.R1, asm.R0, 0, asm.Word).WithSymbol("tagged"),
		asm.StoreMem(asm.RFP, -8, asm.R1, asm.Word),
		asm.LoadMapPtr(asm.R1, execs),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -8),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		asm.LoadMem(asm.R1, asm.R0, 0, asm.DWord),
		asm.Add.Imm(asm.R1, 1),
		asm.StoreMem(asm.R0, 0, asm.R1, asm.DWord),
		asm.Mov.Imm(asm.R0, 0).WithSymbol("exit"),
		asm.Return(),
	)
	return insns, nil
}

// oomProg sends each OOM victim to user space, with its agent and memory.
func oomProg(f map[string]field, tags, events int) (asm.Instructions, error) {
	pid, err := need(f, "pid", 4)
	if err != nil {
		return nil, err
	}
	insns := append(asm.Instructions{asm.Mov.Reg(asm.R6, asm.R1)}, zeroEvent()...)
	insns = append(insns,
		asm.StoreImm(asm.RFP, -72, evOOM, asm.Word),
		asm.LoadMem(asm.R1, asm.R6, pid, asm.Word),
		asm.StoreMem(asm.RFP, -68, asm.R1, asm.Word),
		asm.StoreMem(asm.RFP, -4, asm.R1, asm.Word),
		asm.StoreImm(asm.RFP, -64, -1, asm.Word),
	)
	for i, name := range []string{"anon_rss", "file_rss", "shmem_rss"} {
		off, err := need(f, name, 8)
		if err != nil {
			continue // older kernels: memory unknown
		}
		insns = append(insns,
			asm.LoadMem(asm.R1, asm.R6, off, asm.DWord),
			asm.StoreMem(asm.RFP, int16(-56+8*i), asm.R1, asm.DWord))
	}
	insns = append(insns,
		asm.LoadMapPtr(asm.R1, tags),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -4),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "send"),
		asm.LoadMem(asm.R1, asm.R0, 0, asm.Word),
		asm.StoreMem(asm.RFP, -64, asm.R1, asm.Word),
		asm.LoadMapPtr(asm.R1, events).WithSymbol("send"),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -72),
		asm.Mov.Imm(asm.R3, eventSize),
		asm.Mov.Imm(asm.R4, 0),
		asm.FnRingbufOutput.Call(),
		asm.Mov.Imm(asm.R0, 0),
		asm.Return(),
	)
	return insns, nil
}

func (p *Probe) newEpoch() {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	p.epoch = hex.EncodeToString(b)
}

// slotFor is id's agent slot; when the slots run out the probe starts a new epoch.
func (p *Probe) slotFor(id string) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.slot[id]; ok {
		return s
	}
	if len(p.ids) >= maxAgents {
		log.Printf("probe: %d agents since the epoch began: starting a new one", maxAgents)
		zero := make([]uint64, mustCPUs())
		for i := range uint32(maxAgents) {
			_ = p.cpu.Put(i, zero)
			_ = p.execs.Put(i, zero)
		}
		var k, v uint32
		it := p.tags.Iterate()
		var keys []uint32
		for it.Next(&k, &v) {
			keys = append(keys, k)
		}
		for _, k := range keys {
			_ = p.tags.Delete(k)
		}
		p.ids, p.slot = nil, map[string]uint32{}
		p.newEpoch()
	}
	s := uint32(len(p.ids))
	p.ids = append(p.ids, id)
	p.slot[id] = s
	return s
}

func mustCPUs() int {
	n, err := ebpf.PossibleCPU()
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// tag gives pid its agent when its environment names one; it reports whether it did.
func (p *Probe) tag(pid uint32) bool {
	env, err := os.ReadFile(filepath.Join(p.proc, strconv.Itoa(int(pid)), "environ"))
	if err != nil {
		return false
	}
	id := agentOf(env)
	if id == "" {
		return false
	}
	return p.tags.Update(pid, p.slotFor(id), ebpf.UpdateAny) == nil
}

// scan tags every process with an agent that has none yet: those that started before the probe,
// or whose start the probe missed.
func (p *Probe) scan() {
	entries, _ := os.ReadDir(p.proc)
	var v uint32
	for _, e := range entries {
		pid, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue
		}
		if p.tags.Lookup(uint32(pid), &v) == nil {
			continue
		}
		p.tag(uint32(pid))
	}
}

func agentOf(env []byte) string {
	for _, kv := range bytes.Split(env, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("PASEO_AGENT_ID=")); ok && len(v) > 0 && len(v) <= 128 {
			return string(v)
		}
	}
	return ""
}

func (p *Probe) readEvents() {
	for {
		rec, err := p.rd.Read()
		if err != nil {
			if !errors.Is(err, ringbuf.ErrClosed) {
				log.Printf("probe: events: %v", err)
			}
			return
		}
		b := rec.RawSample
		if len(b) < eventSize {
			continue
		}
		le := binary.LittleEndian // the probe runs only where it was loaded: the host's order
		typ, pid, slot := le.Uint32(b[0:]), le.Uint32(b[4:]), int32(le.Uint32(b[8:]))
		switch typ {
		case evExec:
			p.tag(pid)
		case evOOM:
			o := OOM{At: time.Now().UTC(), PID: int(pid), AnonRSS: le.Uint64(b[16:]) << 10,
				FileRSS: le.Uint64(b[24:]) << 10, ShmemRSS: le.Uint64(b[32:]) << 10}
			if c, err := os.ReadFile(filepath.Join(p.proc, strconv.Itoa(int(pid)), "comm")); err == nil {
				o.Comm = strings.TrimSpace(string(c))
			}
			p.mu.Lock()
			if slot >= 0 && int(slot) < len(p.ids) {
				o.AgentID = p.ids[slot]
			}
			p.seq++
			o.Seq = p.seq
			p.ooms = append(p.ooms, o)
			if len(p.ooms) > oomKeep {
				p.ooms = p.ooms[len(p.ooms)-oomKeep:]
			}
			p.mu.Unlock()
			log.Printf("probe: OOM kill: pid %d (%s), agent %q", o.PID, o.Comm, o.AgentID)
		}
	}
}

// Snapshot is every agent's counters, and the OOM kills after seq after.
func (p *Probe) Snapshot(after uint64) Snapshot {
	p.mu.Lock()
	ids := append([]string(nil), p.ids...)
	s := Snapshot{Version: p.Version, Epoch: p.epoch, Seq: p.seq, Agents: map[string]Counters{}}
	for _, o := range p.ooms {
		if o.Seq > after {
			s.OOMs = append(s.OOMs, o)
		}
	}
	p.mu.Unlock()
	var cpu, execs []uint64
	for i, id := range ids {
		var c Counters
		if p.cpu.Lookup(uint32(i), &cpu) == nil {
			for _, v := range cpu {
				c.CPUNanos += v
			}
		}
		if p.execs.Lookup(uint32(i), &execs) == nil {
			for _, v := range execs {
				c.Execs += v
			}
		}
		s.Agents[id] = c
	}
	var k, v uint32
	it := p.tags.Iterate()
	for it.Next(&k, &v) {
		s.Tracked++
	}
	return s
}
