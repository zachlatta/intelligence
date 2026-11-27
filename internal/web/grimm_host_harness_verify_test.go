package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/fleet"
)

// TestVerifyGrimmHostBrowserHarness serves a throwaway Grimm session for the headless-browser
// check in .factory-scratch/grimm-host-check.mjs. It is skipped unless GRIMM_HOST_HARNESS names
// a directory: it writes server.json there (URL, cookie, session), then follows commands written
// to cmd ("minute <minutes ago> <cpu> <calm|busy>" or "quit"), acknowledging each in ack.
func TestVerifyGrimmHostBrowserHarness(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("GRIMM_HOST_HARNESS")
	if dir == "" {
		t.Skip("GRIMM_HOST_HARNESS not set")
	}
	h, _ := dbServer(t)
	id, _ := readySession(t, h)
	now := time.Now().UTC().Truncate(time.Minute)
	// Seed a stale history: the newest minute is 10 minutes old.
	for i := 40; i >= 10; i-- {
		harnessMinute(t, h, now.Add(-time.Duration(i)*time.Minute), 5+float64(i%7), false)
	}
	srv := serve(t, h)
	info, _ := json.Marshal(map[string]string{"url": srv.URL, "cookieName": sessionCookie,
		"cookieValue": h.(*testServer).cookie.Value, "session": id})
	if err := os.WriteFile(filepath.Join(dir, "server.json"), info, 0o600); err != nil {
		t.Fatal(err)
	}
	cmdPath, ackPath := filepath.Join(dir, "cmd"), filepath.Join(dir, "ack")
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(cmdPath)
		if err != nil || len(b) == 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		_ = os.Remove(cmdPath)
		line := strings.TrimSpace(string(b))
		f := strings.Fields(line)
		switch {
		case len(f) == 1 && f[0] == "quit":
			_ = os.WriteFile(ackPath, []byte(line), 0o600)
			return
		case len(f) == 4 && f[0] == "minute":
			ago, _ := strconv.Atoi(f[1])
			cpu, _ := strconv.ParseFloat(f[2], 64)
			harnessMinute(t, h, time.Now().UTC().Truncate(time.Minute).Add(-time.Duration(ago)*time.Minute), cpu, f[3] == "busy")
		default:
			t.Logf("unknown command %q", line)
		}
		_ = os.WriteFile(ackPath, []byte(line), 0o600)
	}
}

func harnessMinute(t *testing.T, h http.Handler, m time.Time, cpu float64, busy bool) {
	t.Helper()
	rep := verifyHostReport(m, cpu)
	if !busy {
		mm := &rep.Minutes[0]
		mm.MemAvailableMin, mm.DiskAvailableMin, mm.SwapUsedMax = 26<<30, 70<<30, 0
		mm.Load1, mm.Load5, mm.Load15 = 0.5, 0.75, 1
		max := cpu + 10
		mm.CPUMaxPct = &max
		zero := uint64(0)
		mm.OOMKills = &zero
		mm.Filesystems[0].Available = 70 << 30
		rep.OOMs = nil
		low := 0.5
		mm.PSICPUSome, mm.PSIMemSome, mm.PSIMemFull, mm.PSIIOSome, mm.PSIIOFull = &low, &low, &low, &low, &low
		mm.DiskBusyAvgPct, mm.DiskBusyMaxPct = &low, &low
		mm.PaseoOOMKills = &zero
	}
	d := h.(*testServer).d
	if err := fleet.New(d.Owner).RecordHost(t.Context(), "host-1", rep, m); err != nil {
		t.Fatal(err)
	}
}
