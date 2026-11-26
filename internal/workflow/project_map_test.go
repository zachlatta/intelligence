package workflow

import (
	"strings"
	"testing"
)

func TestProjectMapDefinitionValidation(t *testing.T) {
	t.Parallel()

	base := func() Definition {
		return Definition{
			Host:    "box-1",
			Trigger: TriggerManual,
			Project: Project{
				Mode: ProjectMap,
				Routes: []ProjectRoute{
					{Slug: "org/alpha", Directory: "/srv/repos/alpha/", Keys: []string{"AAAA-1111", "aaaa-1111"}},
					{Slug: "org/beta", Directory: "/srv/repos/beta", Keys: []string{"bbbb-2222"}},
				},
			},
			Orchestrator: Orchestrator{Provider: "claude", Instructions: "Orchestrate."},
			Steps:        []Step{{ID: "work", Kind: KindStep, Title: "Work", Instructions: "Do the work."}},
		}
	}

	d := base()
	if err := d.Validate(); err != nil {
		t.Fatalf("valid map definition rejected: %v", err)
	}
	norm := d.normalized()
	if got := norm.Project.Routes[0].Directory; got != "/srv/repos/alpha" {
		t.Fatalf("normalized route[0].Directory = %q, want /srv/repos/alpha", got)
	}
	if len(norm.Project.Routes[0].Keys) != 1 || norm.Project.Routes[0].Keys[0] != "aaaa-1111" {
		t.Fatalf("normalized route[0].Keys = %v, want [aaaa-1111]", norm.Project.Routes[0].Keys)
	}

	cases := map[string]struct {
		mutate func(*Definition)
		want   string
	}{
		"empty routes": {
			func(d *Definition) { d.Project.Routes = nil },
			"at least one route",
		},
		"routes on fixed": {
			func(d *Definition) {
				d.Project.Mode = ProjectFixed
				d.Project.Directory = "/srv/repos/alpha"
			},
			"only valid when project mode is map",
		},
		"mixed directory on map": {
			func(d *Definition) { d.Project.Directory = "/srv/repos/alpha" },
			"must not set fixed directory or finder agent fields",
		},
		"mixed finder provider on map": {
			func(d *Definition) { d.Project.Provider = "claude" },
			"must not set fixed directory or finder agent fields",
		},
		"relative route dir": {
			func(d *Definition) { d.Project.Routes[0].Directory = "repos/alpha" },
			"absolute repository directory",
		},
		"dotdot route dir": {
			func(d *Definition) { d.Project.Routes[0].Directory = "/srv/repos/../etc" },
			"must not contain ..",
		},
		"empty keys": {
			func(d *Definition) { d.Project.Routes[0].Keys = nil },
			"at least one key",
		},
		"blank key": {
			func(d *Definition) { d.Project.Routes[0].Keys = []string{"  "} },
			"empty key",
		},
		"ambiguous duplicate key across repos": {
			func(d *Definition) { d.Project.Routes[1].Keys = append(d.Project.Routes[1].Keys, "AAAA-1111") },
			"mapped to both",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := base()
			tc.mutate(&d)
			err := d.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestResolveProjectRoute(t *testing.T) {
	t.Parallel()

	routes := []ProjectRoute{
		{Slug: "org/warehouse", Directory: "/srv/repos/warehouse", Keys: []string{"root-dw"}},
		{Slug: "org/infra", Directory: "/srv/repos/infra", Keys: []string{"child-sys", "root-sys"}},
		{Slug: "org/outliner", Directory: "/srv/repos/outliner", Keys: []string{"root-outliner"}},
	}

	t.Run("ancestor inheritance", func(t *testing.T) {
		input := "---\ncontract_ids:\n  - leaf-dw-1\ncontract_ancestors:\n  leaf-dw-1:\n    - leaf-dw-1\n    - root-dw\n---\nFix the warehouse contract.\n"
		rt, err := ResolveProjectRoute(routes, input)
		if err != nil {
			t.Fatalf("ResolveProjectRoute: %v", err)
		}
		if rt.Directory != "/srv/repos/warehouse" || rt.Slug != "org/warehouse" {
			t.Fatalf("got %+v, want org/warehouse", rt)
		}
	})

	t.Run("child override beats parent route", func(t *testing.T) {
		input := "---\ncontract_ids:\n  - leaf-under-sys\ncontract_ancestors:\n  leaf-under-sys: [leaf-under-sys, child-sys, root-dw]\n---\nFix the infra child under DW.\n"
		rt, err := ResolveProjectRoute(routes, input)
		if err != nil {
			t.Fatalf("ResolveProjectRoute: %v", err)
		}
		if rt.Directory != "/srv/repos/infra" {
			t.Fatalf("got %+v, want /srv/repos/infra", rt)
		}
	})

	t.Run("same-repo multi-contract batch allowed", func(t *testing.T) {
		input := "---\ncontract_ids:\n  - leaf-1\n  - leaf-2\ncontract_ancestors:\n  leaf-1: [leaf-1, child-sys, root-dw]\n  leaf-2: [leaf-2, root-sys]\n---\nBatch fix.\n"
		rt, err := ResolveProjectRoute(routes, input)
		if err != nil {
			t.Fatalf("ResolveProjectRoute: %v", err)
		}
		if rt.Directory != "/srv/repos/infra" {
			t.Fatalf("got %+v, want /srv/repos/infra", rt)
		}
	})

	t.Run("generic keys supported", func(t *testing.T) {
		input := "---\nkeys: [ROOT-OUTLINER]\n---\nFix outliner.\n"
		rt, err := ResolveProjectRoute(routes, input)
		if err != nil {
			t.Fatalf("ResolveProjectRoute: %v", err)
		}
		if rt.Directory != "/srv/repos/outliner" {
			t.Fatalf("got %+v, want /srv/repos/outliner", rt)
		}
	})

	refusals := map[string]struct {
		input string
		want  string
	}{
		"missing frontmatter": {
			"Fix this contract without frontmatter.",
			"YAML frontmatter",
		},
		"empty contract_ids": {
			"---\ncontract_ids: []\n---\nBody",
			"at least one entry",
		},
		"unmapped key": {
			"---\ncontract_ids:\n  - unknown-uuid\ncontract_ancestors:\n  unknown-uuid: [unknown-uuid, unknown-root]\n---\nBody",
			"no project route matches",
		},
		"mixed-repo batch": {
			"---\ncontract_ids:\n  - leaf-dw\n  - leaf-sys\ncontract_ancestors:\n  leaf-dw: [leaf-dw, root-dw]\n  leaf-sys: [leaf-sys, child-sys, root-dw]\n---\nBody",
			"span multiple repositories",
		},
		"caller-supplied target_repo refused": {
			"---\ncontract_ids: [root-dw]\ntarget_repo: /srv/repos/outliner\n---\nBody",
			"must not specify repository field",
		},
		"caller-supplied target_repo_id refused": {
			"---\ncontract_ids: [root-dw]\ntarget_repo_id: org/outliner\n---\nBody",
			"must not specify repository field",
		},
		"caller-supplied directory refused": {
			"---\ncontract_ids: [root-dw]\ndirectory: /etc\n---\nBody",
			"must not specify repository field",
		},
		"spoofed ancestor chain not starting with contract id": {
			"---\ncontract_ids: [unmapped-leaf]\ncontract_ancestors:\n  unmapped-leaf: [root-dw]\n---\nBody",
			"must begin with",
		},
	}

	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveProjectRoute(routes, tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ResolveProjectRoute() err = %v, want substring %q", err, tc.want)
			}
		})
	}
}
