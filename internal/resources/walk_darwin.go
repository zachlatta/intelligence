package resources

import (
	"encoding/binary"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	darwinCtlKern             = 1  // CTL_KERN
	darwinKernProcArgs2       = 49 // KERN_PROCARGS2
	darwinProcInfoCallPidInfo = 2  // PROC_INFO_CALL_PIDINFO
	darwinProcPidTaskInfo     = 4  // PROC_PIDTASKINFO
)

// darwinProcTaskInfo matches struct proc_taskinfo in Darwin <sys/proc_info.h> (96 bytes).
type darwinProcTaskInfo struct {
	VirtualSize      uint64
	ResidentSize     uint64
	TotalUser        uint64
	TotalSystem      uint64
	ThreadsUser      uint64
	ThreadsSystem    uint64
	Policy           int32
	Faults           int32
	Pageins          int32
	CowFaults        int32
	MessagesSent     int32
	MessagesReceived int32
	SyscallsMach     int32
	SyscallsUnix     int32
	Csw              int32
	ThreadNum        int32
	NumRunning       int32
	Priority         int32
}

// Walk finds each Paseo agent's processes on macOS using direct Darwin syscalls (no root, no
// Full Disk Access, no subprocess spawning): kern.proc.uid for the current user's process table,
// KERN_PROCARGS2 for PASEO_AGENT_ID, PPID ancestor resolution for descendants, and
// PROC_PIDTASKINFO for attributed processes' resident memory and CPU time.
func (w *AgentWalker) Walk() map[string]AgentProcs {
	if w.Proc != "" {
		return w.walkLinux()
	}
	if w.darwinTBFreq == 0 {
		w.darwinTBFreq = darwinTimebaseFreq()
	}
	if len(w.darwinBuf) == 0 {
		w.darwinBuf = make([]byte, darwinArgMax())
	}

	kps, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil || len(kps) == 0 {
		w.prev = nil
		w.darwinAgents = nil
		return map[string]AgentProcs{}
	}

	procs := make([]darwinProc, 0, len(kps))
	nextCache := make(map[int]darwinCachedAgent, len(w.darwinAgents))
	for i := range kps {
		kp := &kps[i]
		pid := int(kp.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		var start uint64
		if kp.Proc.P_starttime.Sec >= 0 && kp.Proc.P_starttime.Usec >= 0 {
			start = uint64(kp.Proc.P_starttime.Sec)*1_000_000 + uint64(kp.Proc.P_starttime.Usec)
		}
		var agentID string
		if cached, ok := w.darwinAgents[pid]; ok && cached.start == start && cached.agentID != "" {
			agentID = cached.agentID
		} else if raw, ok := w.readProcArgs2(pid); ok {
			agentID = parseDarwinProcArgs2(raw)
		}
		if agentID != "" {
			nextCache[pid] = darwinCachedAgent{start: start, agentID: agentID}
		}
		procs = append(procs, darwinProc{
			pid:     pid,
			ppid:    int(kp.Eproc.Ppid),
			start:   start,
			agentID: agentID,
		})
	}
	w.darwinAgents = nextCache

	resolveDarwinAgents(procs)

	// Query PROC_PIDTASKINFO only for processes attributed to an agent.
	for i := range procs {
		if procs[i].agentID == "" {
			continue
		}
		if info, ok := darwinTaskInfo(procs[i].pid); ok {
			procs[i].mem = info.ResidentSize
			procs[i].ticks = machToTicks(info.TotalUser+info.TotalSystem, w.darwinTBFreq)
			procs[i].hasTask = true
		}
	}

	out, nextPrev := aggregateDarwinProcs(procs, w.prev)
	w.prev = nextPrev
	return out
}

func (w *AgentWalker) readProcArgs2(pid int) ([]byte, bool) {
	mib := [3]int32{darwinCtlKern, darwinKernProcArgs2, int32(pid)}
	n := uintptr(len(w.darwinBuf))
	_, _, errno := unix.Syscall6(
		unix.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])),
		3,
		uintptr(unsafe.Pointer(&w.darwinBuf[0])),
		uintptr(unsafe.Pointer(&n)),
		0,
		0,
	)
	if errno != 0 {
		return nil, false
	}
	return darwinProcArgsSlice(w.darwinBuf, int(n))
}

func darwinTaskInfo(pid int) (darwinProcTaskInfo, bool) {
	var info darwinProcTaskInfo
	r1, _, errno := unix.Syscall6(
		unix.SYS_PROC_INFO,
		darwinProcInfoCallPidInfo,
		uintptr(pid),
		darwinProcPidTaskInfo,
		0,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if errno != 0 || int(r1) < int(unsafe.Sizeof(info)) {
		return darwinProcTaskInfo{}, false
	}
	return info, true
}

func darwinTimebaseFreq() uint64 {
	if raw, err := unix.SysctlRaw("hw.tbfrequency"); err == nil {
		if len(raw) == 8 {
			if v := binary.LittleEndian.Uint64(raw); v > 0 {
				return v
			}
		} else if len(raw) == 4 {
			if v := binary.LittleEndian.Uint32(raw); v > 0 {
				return uint64(v)
			}
		}
	}
	if runtime.GOARCH == "arm64" {
		return 24_000_000
	}
	return 1_000_000_000
}

func darwinArgMax() int {
	v, err := unix.SysctlUint32("kern.argmax")
	return darwinProcArgsBufSize(v, err == nil)
}
