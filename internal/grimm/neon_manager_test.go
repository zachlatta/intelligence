package grimm

import (
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// withNeon gives the rig's account an Antigravity plan with headroom, Gemini 4 Neon Medium in
// host-1's catalog and in a model group, next to the rig's Claude Opus 5.5 and Codex.
func (r *rig) withNeon() {
	r.t.Helper()
	ctx := r.t.Context()
	r.exec(`UPDATE provider_plan_settings SET enabled = true WHERE provider = 'antigravity'`)
	r.exec(`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES('antigravity','default',now(),now())`)
	r.exec(`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1','antigravity','default',true,now())`)
	r.exec(`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed)
		VALUES('antigravity','default','gemini/7d',604800,20,now()+interval '1 day',now(),now())`)
	r.exec(`INSERT INTO usage_active(host,provider,account,since,until) VALUES('host-1','antigravity','default',now(),now())`)
	rep := hostclient.Report{Name: "host-1", Version: "v", Paseo: hostclient.PaseoStatus{Connected: true,
		Providers: []hostclient.Provider{
			{Name: "claude", Available: true, Models: []hostclient.Model{{ID: "claude-opus-5-5", Thinking: []string{"medium"}}}},
			{Name: "codex", Available: true, Models: []hostclient.Model{{ID: "gpt-6.2-sol", Thinking: []string{"high"}}}},
			{Name: "antigravity", Available: true, Models: []hostclient.Model{{ID: "gemini-4-neon-medium"}}},
		},
		Workspaces: []hostclient.Workspace{{ID: "ws-1", Name: "grimm", Kind: "worktree", Status: "running", Directory: "/w/app-grimm"}}}}
	if err := r.reg.Record(ctx, rep, time.Now()); err != nil {
		r.t.Fatal(err)
	}
	wf := workflow.NewEngine(r.d.Owner, time.Now)
	if _, err := wf.SaveModelGroup(ctx, r.acct, "", workflow.ModelGroup{Name: "Neon", Members: []workflow.ModelMember{
		{Provider: "antigravity", Model: "gemini-4-neon-medium", Login: "default", LongWindow: "gemini/7d", Enabled: true},
	}}); err != nil {
		r.t.Fatal(err)
	}
}

func TestTheManagerDefaultsToOpus55(t *testing.T) {
	t.Parallel()
	if ManagerProvider != "claude" || ManagerModel != "claude-opus-5-5" {
		t.Errorf("manager = %s/%s, want claude/claude-opus-5-5", ManagerProvider, ManagerModel)
	}
	p := PolicyFor("manager")
	if p.Preferred != "claude" || p.Pinned["claude"] != "claude-opus-5-5" {
		t.Errorf("manager policy = %+v, want claude pinned to claude-opus-5-5", p)
	}
}

func TestTheManagerCanBeSelectedAsNeonMediumWithoutAThinkingLevel(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.withNeon()
	ctx := t.Context()
	id, err := r.s.CreateSession(ctx, r.acct, NewSession{
		Host: "host-1", Repo: "/w/app",
		ManagerProvider: "antigravity", ManagerModel: "gemini-4-neon-medium", ManagerThinking: "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	r.succeed(r.take("create_workspace"), `{"workspaceId":"ws-1"}`)
	r.reconcile(id)
	if got := r.one(`SELECT count(*) FROM grimm_quota WHERE provider = 'antigravity' AND model = 'gemini-4-neon-medium' AND eligible`); got != int64(1) {
		t.Fatalf("rig: Neon Medium is not an eligible plan (%v rows): %v", got,
			r.one(`SELECT coalesce(string_agg(provider||'/'||model||' '||eligible||' '||coalesce(reason,''), '; '), '') FROM grimm_quota`))
	}
	mgr := r.take("create_agent")
	if mgr.Args["provider"] != "antigravity" || mgr.Args["model"] != "gemini-4-neon-medium" {
		t.Fatalf("manager started on %v/%v, want antigravity/gemini-4-neon-medium (args %+v)", mgr.Args["provider"], mgr.Args["model"], mgr.Args)
	}
	// Antigravity picks effort through the model id; Paseo rejects a thinking option for it.
	if th, _ := mgr.Args["thinking"].(string); th != "" {
		t.Errorf("manager thinking = %q, want empty for antigravity", th)
	}
	if mgr.Args["mode"] != "full-access" {
		t.Errorf("manager mode = %v, want antigravity's full-access", mgr.Args["mode"])
	}
	if got := r.one(`SELECT provider || '/' || model FROM grimm_managers ORDER BY epoch DESC LIMIT 1`); got != "antigravity/gemini-4-neon-medium" {
		t.Errorf("grimm_managers records %v", got)
	}
}

func TestTheDefaultManagerPolicyPinsOpus55(t *testing.T) {
	t.Parallel()
	cands := []Candidate{
		{Provider: "claude", Model: "claude-opus-5-5", Eligible: true},
		{Provider: "antigravity", Model: "gemini-4-neon-medium", Eligible: true},
		{Provider: "antigravity", Model: "gemini-4-neon-xhigh", Eligible: true},
	}
	ok := map[string]bool{}
	for _, c := range Judge("manager", cands, nil) {
		ok[c.Spec()] = c.Eligible
	}
	if !ok["claude/claude-opus-5-5"] {
		t.Errorf("manager may not run on Opus 5.5: %v", ok)
	}
	if ok["antigravity/gemini-4-neon-medium"] || ok["antigravity/gemini-4-neon-xhigh"] {
		t.Errorf("default manager policy allowed non-Opus model: %v", ok)
	}
}

func TestReviewersAndVerifiersStayOnOpus(t *testing.T) {
	t.Parallel()
	cands := []Candidate{
		{Provider: "claude", Model: "claude-opus-5-5", Eligible: true},
		{Provider: "claude", Model: "claude-sonnet-5-5", Eligible: true},
		{Provider: "antigravity", Model: "gemini-4-neon-medium", Eligible: true},
		{Provider: "antigravity", Model: "claude-opus-5-5", Eligible: true},
		{Provider: "codex", Model: "gpt-6.2-sol", Eligible: true},
	}
	judge := func(role string) map[string]bool {
		ok := map[string]bool{}
		for _, c := range Judge(role, cands, nil) {
			ok[c.Spec()] = c.Eligible
		}
		return ok
	}
	for _, role := range []string{"reviewer", "checker", "audit", "auditor"} {
		ok := judge(role)
		for spec, e := range ok {
			if e != (spec == "claude/claude-opus-5-5") {
				t.Errorf("%s on %s eligible=%v, want only claude/claude-opus-5-5", role, spec, e)
			}
		}
		if p := PolicyFor(role); p.Pinned["claude"] != "claude-opus-5-5" || p.Preferred != "claude" {
			t.Errorf("%s policy = %+v", role, p)
		}
	}
	ok := judge("verifier")
	if !ok["claude/claude-opus-5-5"] || ok["claude/claude-sonnet-5-5"] || ok["antigravity/gemini-4-neon-medium"] || ok["antigravity/claude-opus-5-5"] {
		t.Errorf("verifier eligibility = %v, want Opus 5.5 on claude, never Gemini or antigravity", ok)
	}
	if !ok["codex/gpt-6.2-sol"] {
		t.Errorf("verifier lost codex: %v", ok)
	}
	if p := PolicyFor("verifier"); p.Preferred != "claude" || p.Pinned["claude"] != "claude-opus-5-5" {
		t.Errorf("verifier policy = %+v", p)
	}
}

func TestAReviewerHireStaysOnOpusWithNeonAvailable(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.withNeon()
	ctx := t.Context()
	id, err := r.s.CreateSession(ctx, r.acct, NewSession{Host: "host-1", Repo: "/w/app"})
	if err != nil {
		t.Fatal(err)
	}
	r.succeed(r.take("create_workspace"), `{"workspaceId":"ws-1"}`)
	r.reconcile(id)
	for _, role := range []string{"reviewer", "verifier"} {
		w, err := r.s.HireWorker(ctx, r.acct, id, Hire{Role: role, Task: "check it"})
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if w.Provider != "claude" || w.Model != "claude-opus-5-5" {
			t.Errorf("%s hired on %s/%s, want claude/claude-opus-5-5", role, w.Provider, w.Model)
		}
		if _, err := r.s.HireWorker(ctx, r.acct, id, Hire{Role: role, Provider: "antigravity", Task: "check it"}); err == nil {
			t.Errorf("a %s was hired on antigravity", role)
		}
	}
}
