package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/usage"
)

func TestUnsubscribedIsNotAnActivePlanEvenAfterFreshReports(t *testing.T) {
	for _, provider := range []string{usage.Muse, usage.Antigravity, "gemini"} {
		t.Run(provider, func(t *testing.T) {
			r := usageRig(t)
			now := time.Now().UTC()
			recordUsage(t, r, "host-1", usage.Report{CollectedAt: now, Sources: []usage.Source{
				{Provider: provider, Accounts: []usage.Account{{ID: "default", Active: true, Windows: []usage.Window{week(10, now.Add(time.Hour))}}}},
				{Provider: provider, Error: "subscription ended"},
			}})
			v, err := r.Plans(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(v.Plans) != 0 || len(v.Problems) != 0 {
				t.Errorf("disabled plans: %+v", v)
			}
			var n int
			if err := r.p.QueryRow(ctx, `SELECT count(*) FROM usage_totals WHERE provider='`+provider+`'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%s counted in totals", provider)
			}
			if _, err := r.p.Exec(ctx, `INSERT INTO accounts(username,password_hash,role_name) VALUES('test','-','test_role');
 INSERT INTO model_groups(account_id,name,token) SELECT id,'Frontier','frontier_model' FROM accounts;
 INSERT INTO hosts(name,last_seen) VALUES('host-1',now());
 INSERT INTO host_keys(host,account_id,key_hash,created_at) SELECT 'host-1',id,'test',now() FROM accounts;
 INSERT INTO model_group_members(account_id,group_id,position,provider,model,thinking,login,long_window)
 SELECT account_id,id,0,'`+provider+`','wonder','high','default','7d' FROM model_groups`); err != nil {
				t.Fatal(err)
			}
			var reason string
			var eligible bool
			if err := r.p.QueryRow(ctx, `SELECT reason,eligible FROM model_group_candidates`).Scan(&reason, &eligible); err != nil {
				t.Fatal(err)
			}
			if eligible || reason != "Provider disabled" {
				t.Errorf("candidate: %s %v", reason, eligible)
			}
			if err := r.p.QueryRow(ctx, `SELECT count(*) FROM usage_readings WHERE provider='`+provider+`'`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("history lost: %d %v", n, err)
			}
		})
	}
}

func TestUnsubscribedLaunchCommandsAreRefused(t *testing.T) {
	for _, provider := range []string{usage.Muse, usage.Antigravity, "gemini"} {
		t.Run(provider, func(t *testing.T) {
			r := commandRig(t)
			for kind, args := range map[string]string{
				"create_agent":     `{"provider":"` + provider + `","cwd":"/w"}`,
				"create_workspace": `{"cwd":"/w","agent":{"provider":"` + provider + `"}}`,
				"create_schedule":  `{"prompt":"test","every":"1h","agent":{"provider":"` + provider + `","cwd":"/w"}}`,
			} {
				_, err := r.Enqueue(ctx, "host-1", kind, json.RawMessage(args), time.Minute, t1)
				if err == nil || !strings.Contains(err.Error(), "disabled") {
					t.Errorf("%s: %v", kind, err)
				}
			}
		})
	}
}

func TestUnsubscribedSchedulesArePausedOnReport(t *testing.T) {
	for _, provider := range []string{usage.Muse, usage.Antigravity, "gemini"} {
		t.Run(provider, func(t *testing.T) {
			r := commandRig(t)
			err := r.RecordSchedules(ctx, "host-1", hostclient.ScheduleReport{Schedules: []hostclient.Schedule{{ID: "old", Provider: provider, Status: "active", Target: "new-agent"}}}, t1)
			if err != nil {
				t.Fatal(err)
			}
			var n int
			if err := r.p.QueryRow(ctx, `SELECT count(*) FROM commands WHERE kind='pause_schedule' AND args->>'scheduleId'='old'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("pause commands: %d", n)
			}
		})
	}
}
