package workflow

import (
	"strings"
	"testing"
)

const (
	testCommitMain = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testRootUUID   = "10000000-0000-4000-8000-000000000001"
	testDWUUID     = "10000000-0000-4000-8000-000000000010"
	testDWLeafUUID = "10000000-0000-4000-8000-000000000011"
	testSysChildID = "10000000-0000-4000-8000-000000000015"
	testSysLeafID  = "10000000-0000-4000-8000-000000000016"
	testSysRootID  = "10000000-0000-4000-8000-000000000020"
	testSysRootLf  = "10000000-0000-4000-8000-000000000021"
	testUnmapRoot  = "10000000-0000-4000-8000-000000000090"
	testUnmapLeaf  = "10000000-0000-4000-8000-000000000091"
)

const testGitOutline = `- Contracts <!-- id:10000000-0000-4000-8000-000000000001 -->
  - Data Warehouse <!-- id:10000000-0000-4000-8000-000000000010 -->
    - Warehouse pipeline stays fresh <!-- id:10000000-0000-4000-8000-000000000011 -->
    - Warehouse host firewall <!-- id:10000000-0000-4000-8000-000000000015 -->
      - SSH port locked to tailnet <!-- id:10000000-0000-4000-8000-000000000016 -->
  - Infra <!-- id:10000000-0000-4000-8000-000000000020 -->
    - Host backups verified <!-- id:10000000-0000-4000-8000-000000000021 -->
  - Unmapped Sandbox <!-- id:10000000-0000-4000-8000-000000000090 -->
    - Unmapped leaf <!-- id:10000000-0000-4000-8000-000000000091 -->
`

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
				TreeSource: &ProjectTreeSource{
					Directory: "/srv/repos/demo-contracts/",
					Path:      "outliner/outline.md",
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
	if got := norm.Project.TreeSource.Directory; got != "/srv/repos/demo-contracts" {
		t.Fatalf("normalized treeSource.Directory = %q, want /srv/repos/demo-contracts", got)
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
				d.Project.TreeSource = nil
			},
			"only valid when project mode is map",
		},
		"treeSource on fixed": {
			func(d *Definition) {
				d.Project.Mode = ProjectFixed
				d.Project.Directory = "/srv/repos/alpha"
				d.Project.Routes = nil
			},
			"only valid when project mode is map",
		},
		"treeSource relative dir": {
			func(d *Definition) { d.Project.TreeSource.Directory = "repos/demo-contracts" },
			"clean absolute repository directory",
		},
		"treeSource dotdot path": {
			func(d *Definition) { d.Project.TreeSource.Path = "../secret.md" },
			"clean repo-relative file path",
		},
		"treeSource colon in path": {
			func(d *Definition) { d.Project.TreeSource.Path = "HEAD:outliner/outline.md" },
			"clean repo-relative file path",
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

func TestResolveProjectRouteWithVerifiedTree(t *testing.T) {
	t.Parallel()

	routes := []ProjectRoute{
		{Slug: "org/warehouse", Directory: "/srv/repos/warehouse", Keys: []string{testDWUUID}},
		{Slug: "org/infra", Directory: "/srv/repos/infra", Keys: []string{testSysChildID, testSysRootID}},
	}
	ts := &ProjectTreeSource{Directory: "/srv/repos/demo-contracts", Path: "outliner/outline.md"}

	t.Run("YAML block contract_tree ancestor inheritance", func(t *testing.T) {
		input := "---\ncontract_ids:\n  - " + testDWLeafUUID + "\noutline_commit_sha: " + testCommitMain + "\ncontract_tree:\n  - id: " + testRootUUID + "\n    children:\n      - id: " + testDWUUID + "\n        children:\n          - id: " + testDWLeafUUID + "\n---\nFix the warehouse contract.\n"
		rt, err := ResolveProjectRoute(routes, ts, input, testGitOutline, testCommitMain)
		if err != nil {
			t.Fatalf("ResolveProjectRoute: %v", err)
		}
		if rt.Directory != "/srv/repos/warehouse" || rt.Slug != "org/warehouse" {
			t.Fatalf("got %+v, want org/warehouse", rt)
		}
	})

	t.Run("child override beats parent route via verified tree", func(t *testing.T) {
		input := "---\ncontract_ids: [" + testSysLeafID + "]\noutline_commit_sha: " + testCommitMain + "\ncontract_tree: [{\"id\":\"" + testRootUUID + "\",\"children\":[{\"id\":\"" + testDWUUID + "\",\"children\":[{\"id\":\"" + testSysChildID + "\",\"children\":[{\"id\":\"" + testSysLeafID + "\"}]}]}]}]\n---\nFix firewall.\n"
		rt, err := ResolveProjectRoute(routes, ts, input, testGitOutline, testCommitMain)
		if err != nil {
			t.Fatalf("ResolveProjectRoute: %v", err)
		}
		if rt.Directory != "/srv/repos/infra" {
			t.Fatalf("got %+v, want /srv/repos/infra", rt)
		}
	})

	t.Run("same-repo multi-contract batch across two branches", func(t *testing.T) {
		input := "---\ncontract_ids:\n  - " + testSysLeafID + "\n  - " + testSysRootLf + "\noutline_commit_sha: " + testCommitMain + "\ncontract_tree:\n  - id: " + testRootUUID + "\n    children:\n      - id: " + testDWUUID + "\n        children:\n          - id: " + testSysChildID + "\n            children:\n              - id: " + testSysLeafID + "\n      - id: " + testSysRootID + "\n        children:\n          - id: " + testSysRootLf + "\n---\nBatch fix.\n"
		rt, err := ResolveProjectRoute(routes, ts, input, testGitOutline, testCommitMain)
		if err != nil {
			t.Fatalf("ResolveProjectRoute: %v", err)
		}
		if rt.Directory != "/srv/repos/infra" {
			t.Fatalf("got %+v, want /srv/repos/infra", rt)
		}
	})

	t.Run("Z-F1 indented --- inside special_input block scalar does not close frontmatter early", func(t *testing.T) {
		input := "---\nschema: demo_contracts_v1\ncontract_ids:\n  - " + testDWLeafUUID + "\noutline_commit_sha: " + testCommitMain + "\nspecial_input: |-\n  hello\n  ---\n  target_repo: /srv/repos/infra\ncontract_tree:\n  - id: " + testRootUUID + "\n    children:\n      - id: " + testDWUUID + "\n        children:\n          - id: " + testDWLeafUUID + "\n---\nBody.\n"
		rt, err := ResolveProjectRoute(routes, ts, input, testGitOutline, testCommitMain)
		if err != nil {
			t.Fatalf("Z-F1 indented --- failed: %v", err)
		}
		if rt.Directory != "/srv/repos/warehouse" {
			t.Fatalf("got %+v, want /srv/repos/warehouse", rt)
		}
	})

	t.Run("direct exact keys supported when treeSource is nil", func(t *testing.T) {
		input := "---\nkeys: [" + strings.ToUpper(testDWUUID) + "]\n---\nDirect fix.\n"
		rt, err := ResolveProjectRoute(routes, nil, input, "", "")
		if err != nil {
			t.Fatalf("ResolveProjectRoute(nil treeSource): %v", err)
		}
		if rt.Directory != "/srv/repos/warehouse" {
			t.Fatalf("got %+v, want /srv/repos/warehouse", rt)
		}
	})

	refusals := map[string]struct {
		treeSource *ProjectTreeSource
		input      string
		outline    string
		commit     string
		want       string
	}{
		"P1 legacy contract_ancestors refused": {
			ts,
			"---\ncontract_ids: [" + testUnmapLeaf + "]\ncontract_ancestors:\n  " + testUnmapLeaf + ": [" + testUnmapLeaf + ", " + testDWUUID + "]\n---\nBody",
			testGitOutline, testCommitMain,
			"unverified ancestor chains",
		},
		"P1 spoofed contract_tree placing unmapped under warehouse refused": {
			ts,
			"---\ncontract_ids: [" + testUnmapLeaf + "]\noutline_commit_sha: " + testCommitMain + "\ncontract_tree:\n  - id: " + testRootUUID + "\n    children:\n      - id: " + testDWUUID + "\n        children:\n          - id: " + testUnmapLeaf + "\n---\nBody",
			testGitOutline, testCommitMain,
			"does not match Git outline",
		},
		"P2 spoofed contract_tree placing warehouse leaf under infra refused": {
			ts,
			"---\ncontract_ids: [" + testDWLeafUUID + "]\noutline_commit_sha: " + testCommitMain + "\ncontract_tree:\n  - id: " + testRootUUID + "\n    children:\n      - id: " + testSysRootID + "\n        children:\n          - id: " + testDWLeafUUID + "\n---\nBody",
			testGitOutline, testCommitMain,
			"does not match Git outline",
		},
		"P3 partial subtree not rooted at Git root refused": {
			ts,
			"---\ncontract_ids: [" + testDWLeafUUID + "]\noutline_commit_sha: " + testCommitMain + "\ncontract_tree:\n  - id: " + testDWUUID + "\n    children:\n      - id: " + testDWLeafUUID + "\n---\nBody",
			testGitOutline, testCommitMain,
			"does not match Git outline",
		},
		"P4 double-quoted forbidden target_repo refused (Z-F1)": {
			ts,
			"---\ncontract_ids: [" + testDWLeafUUID + "]\n\"target_repo\": /srv/repos/infra\noutline_commit_sha: " + testCommitMain + "\ncontract_tree:\n  - id: " + testRootUUID + "\n---\nBody",
			testGitOutline, testCommitMain,
			"must not specify repository field",
		},
		"P4 single-quoted forbidden directory refused (Z-F1)": {
			nil,
			"---\ncontract_ids: [" + testDWUUID + "]\n'directory': /etc\n---\nBody",
			"", "",
			"must not specify repository field",
		},
		"P6 missing frontmatter": {
			ts,
			"Fix contract without frontmatter.",
			testGitOutline, testCommitMain,
			"YAML frontmatter",
		},
		"P7 both contract_ids and keys": {
			nil,
			"---\ncontract_ids: [" + testDWUUID + "]\nkeys: [" + testSysRootID + "]\n---\nBody",
			"", "",
			"must not specify both",
		},
		"missing contract_tree when treeSource configured": {
			ts,
			"---\ncontract_ids: [" + testDWUUID + "]\noutline_commit_sha: " + testCommitMain + "\n---\nBody",
			testGitOutline, testCommitMain,
			"requires contract_tree",
		},
		"contract_tree refused when treeSource is nil": {
			nil,
			"---\ncontract_ids: [" + testDWLeafUUID + "]\ncontract_tree: [{\"id\":\"" + testRootUUID + "\"}]\n---\nBody",
			"", "",
			"requires project.treeSource",
		},
		"unmapped contract with real Git tree refused": {
			ts,
			"---\ncontract_ids: [" + testUnmapLeaf + "]\noutline_commit_sha: " + testCommitMain + "\ncontract_tree:\n  - id: " + testRootUUID + "\n    children:\n      - id: " + testUnmapRoot + "\n        children:\n          - id: " + testUnmapLeaf + "\n---\nBody",
			testGitOutline, testCommitMain,
			"no project route matches",
		},
		"mixed-repo batch with real Git tree refused": {
			ts,
			"---\ncontract_ids:\n  - " + testDWLeafUUID + "\n  - " + testSysRootLf + "\noutline_commit_sha: " + testCommitMain + "\ncontract_tree:\n  - id: " + testRootUUID + "\n    children:\n      - id: " + testDWUUID + "\n        children:\n          - id: " + testDWLeafUUID + "\n      - id: " + testSysRootID + "\n        children:\n          - id: " + testSysRootLf + "\n---\nBody",
			testGitOutline, testCommitMain,
			"span multiple repositories",
		},
		"unverified / missing Git outline fails closed": {
			ts,
			"---\ncontract_ids: [" + testDWLeafUUID + "]\noutline_commit_sha: " + testCommitMain + "\ncontract_tree:\n  - id: " + testRootUUID + "\n---\nBody",
			"", "",
			"refusing unverified contract_tree",
		},
	}

	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveProjectRoute(routes, tc.treeSource, tc.input, tc.outline, tc.commit)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ResolveProjectRoute() err = %v, want substring %q", err, tc.want)
			}
		})
	}
}
