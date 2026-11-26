package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// forbiddenMapHeaderKeys are caller-supplied repository selection fields that mapped workflows
// must refuse so external callers cannot steer a run's target repository.
var forbiddenMapHeaderKeys = map[string]bool{
	"target_repo":         true,
	"target_repo_id":      true,
	"target_repo_slug":    true,
	"repo":                true,
	"directory":           true,
	"project_directory":   true,
	"workspace_directory": true,
}

var (
	gitCommitSHARe  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	outlineBulletRe = regexp.MustCompile(`^( *)[-*+](?: (.*))?$`)
	outlineMetaIDRe = regexp.MustCompile(` ?<!-- id:([0-9a-fA-F-]{36})( collapsed)?(?: pending:(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ))? -->$`)
)

// ContractTreeNode is one node in a caller-supplied contract tree.
type ContractTreeNode struct {
	ID       string             `json:"id"`
	Children []ContractTreeNode `json:"children,omitempty"`
}

func validateProjectRoutes(routes []ProjectRoute) error {
	if len(routes) == 0 {
		return fmt.Errorf("a mapped project needs at least one route")
	}
	keyToDir := map[string]string{}
	for i, rt := range routes {
		n := i + 1
		rawDir := strings.TrimSpace(rt.Directory)
		if rawDir == "" || !path.IsAbs(rawDir) {
			return fmt.Errorf("project route %d needs an absolute repository directory on the host", n)
		}
		if slices.Contains(strings.Split(rawDir, "/"), "..") {
			return fmt.Errorf("project route %d: directory %q must not contain .. segments", n, rt.Directory)
		}
		cleanDir := path.Clean(rawDir)
		if len(rt.Keys) == 0 {
			return fmt.Errorf("project route %d (%s) needs at least one key", n, cleanDir)
		}
		validKeys := 0
		for _, rawKey := range rt.Keys {
			k := strings.ToLower(strings.TrimSpace(rawKey))
			if k == "" {
				return fmt.Errorf("project route %d (%s) contains an empty key", n, cleanDir)
			}
			if prev, exists := keyToDir[k]; exists && prev != cleanDir {
				return fmt.Errorf("project route %d: key %q is mapped to both %s and %s", n, k, prev, cleanDir)
			}
			keyToDir[k] = cleanDir
			validKeys++
		}
		if validKeys == 0 {
			return fmt.Errorf("project route %d (%s) needs at least one key", n, cleanDir)
		}
	}
	return nil
}

func validateProjectTreeSource(ts ProjectTreeSource) error {
	dir := strings.TrimSpace(ts.Directory)
	if dir == "" || !path.IsAbs(dir) || slices.Contains(strings.Split(dir, "/"), "..") {
		return fmt.Errorf("project treeSource needs a clean absolute repository directory on the host")
	}
	rel := strings.TrimSpace(ts.Path)
	if rel == "" || path.IsAbs(rel) || strings.Contains(rel, ":") || slices.Contains(strings.Split(rel, "/"), "..") || path.Clean(rel) != rel || rel == "." {
		return fmt.Errorf("project treeSource needs a clean repo-relative file path")
	}
	return nil
}

type parsedRoutingHeader struct {
	ids       []string
	tree      []ContractTreeNode
	hasTree   bool
	commitSHA string
}

// ResolveProjectRoute deterministically resolves a mapped workflow's single target repository from
// structured YAML frontmatter at the top of input.
//
// When treeSource is configured, input must supply contract_tree and a 40-hex outline_commit_sha;
// Intelligence verifies every node and parent-child edge in contract_tree against verifiedOutline
// (independently read from Git at verifiedCommit on origin's default branch) before walking the
// verified tree most-specific-first against routes.
//
// When treeSource is nil, Intelligence routes strictly by direct exact match of each key in
// contract_ids or keys against routes and refuses any caller-supplied tree or ancestor chain.
func ResolveProjectRoute(routes []ProjectRoute, treeSource *ProjectTreeSource, input, verifiedOutline, verifiedCommit string) (ProjectRoute, error) {
	if err := validateProjectRoutes(routes); err != nil {
		return ProjectRoute{}, bad("%s", err)
	}
	if treeSource != nil {
		if err := validateProjectTreeSource(*treeSource); err != nil {
			return ProjectRoute{}, bad("%s", err)
		}
	}
	hdr, err := parseRoutingHeader(input)
	if err != nil {
		return ProjectRoute{}, err
	}

	index := make(map[string]ProjectRoute)
	for _, rt := range routes {
		cleanDir := path.Clean(strings.TrimSpace(rt.Directory))
		norm := ProjectRoute{
			Directory: cleanDir,
			Slug:      strings.TrimSpace(rt.Slug),
		}
		for _, rawKey := range rt.Keys {
			k := strings.ToLower(strings.TrimSpace(rawKey))
			if k != "" {
				index[k] = norm
			}
		}
	}

	var treeParent map[string]string
	var inTree map[string]bool
	if treeSource != nil {
		if !hdr.hasTree || len(hdr.tree) == 0 {
			return ProjectRoute{}, bad("mapped workflow with treeSource requires contract_tree in leading YAML frontmatter")
		}
		if !gitCommitSHARe.MatchString(hdr.commitSHA) {
			return ProjectRoute{}, bad("mapped workflow with treeSource requires a 40-character hex outline_commit_sha in leading YAML frontmatter")
		}
		if verifiedCommit == "" || verifiedCommit != hdr.commitSHA || strings.TrimSpace(verifiedOutline) == "" {
			return ProjectRoute{}, bad("authoritative Git outline at commit %s is unavailable; refusing unverified contract_tree", hdr.commitSHA)
		}
		gitParent, gitExists, err := parseOutlineTree(verifiedOutline)
		if err != nil {
			return ProjectRoute{}, bad("invalid authoritative Git outline at commit %s: %s", hdr.commitSHA, err)
		}
		treeParent = map[string]string{}
		inTree = map[string]bool{}
		var verifyWalk func(nodes []ContractTreeNode, expectedParent string) error
		verifyWalk = func(nodes []ContractTreeNode, expectedParent string) error {
			for _, n := range nodes {
				id := strings.ToLower(strings.TrimSpace(n.ID))
				if id == "" {
					return bad("contract_tree contains a node with an empty id")
				}
				if inTree[id] {
					return bad("contract_tree contains duplicate node %q", id)
				}
				if !gitExists[id] {
					return bad("contract_tree node %q does not exist in Git outline at commit %s", id, hdr.commitSHA)
				}
				if gitParent[id] != expectedParent {
					return bad("contract_tree parent for %q does not match Git outline at commit %s", id, hdr.commitSHA)
				}
				inTree[id] = true
				treeParent[id] = expectedParent
				if err := verifyWalk(n.Children, id); err != nil {
					return err
				}
			}
			return nil
		}
		if err := verifyWalk(hdr.tree, ""); err != nil {
			return ProjectRoute{}, err
		}
	} else if hdr.hasTree {
		return ProjectRoute{}, bad("contract_tree requires project.treeSource so Intelligence can verify the tree against Git")
	}

	var chosen *ProjectRoute
	for _, id := range hdr.ids {
		var matched *ProjectRoute
		if treeSource != nil {
			if !inTree[id] {
				return ProjectRoute{}, bad("contract %q is missing from contract_tree", id)
			}
			for cur := id; cur != ""; cur = treeParent[cur] {
				if rt, ok := index[cur]; ok {
					cp := rt
					matched = &cp
					break
				}
			}
		} else {
			if rt, ok := index[id]; ok {
				cp := rt
				matched = &cp
			}
		}
		if matched == nil {
			return ProjectRoute{}, bad("no project route matches %q; add a reviewed route to the workflow definition before starting a run", id)
		}
		if chosen == nil {
			chosen = matched
		} else if chosen.Directory != matched.Directory {
			return ProjectRoute{}, bad("input keys span multiple repositories (%s and %s); split into one run per repository",
				formatRouteLabel(*chosen), formatRouteLabel(*matched))
		}
	}
	if chosen == nil {
		return ProjectRoute{}, bad("mapped project input needs at least one routing key in leading YAML frontmatter")
	}
	return *chosen, nil
}

func formatRouteLabel(rt ProjectRoute) string {
	if rt.Slug != "" {
		return fmt.Sprintf("%s (%s)", rt.Slug, rt.Directory)
	}
	return rt.Directory
}

// parseOutlineTree parses an Outliner Markdown outline (bullet indentation + <!-- id:<uuid> -->)
// into authoritative parent[id] -> parentID ("" for top-level roots) and exists[id].
func parseOutlineTree(md string) (map[string]string, map[string]bool, error) {
	parent := map[string]string{}
	exists := map[string]bool{}
	type frame struct {
		indent int
		id     string
	}
	stack := []frame{{-1, ""}}
	for _, raw := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		line := expandLeadingTabs(raw)
		m := outlineBulletRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent := len(m[1])
		id := ""
		if mm := outlineMetaIDRe.FindStringSubmatch(m[2]); mm != nil {
			id = strings.ToLower(mm[1])
		}
		for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parentID := stack[len(stack)-1].id
		if id != "" {
			if exists[id] {
				return nil, nil, fmt.Errorf("duplicate contract id %q in outline", id)
			}
			exists[id] = true
			parent[id] = parentID
		}
		stack = append(stack, frame{indent: indent, id: id})
	}
	return parent, exists, nil
}

func expandLeadingTabs(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i == 0 || !strings.Contains(s[:i], "\t") {
		return s
	}
	return strings.ReplaceAll(s[:i], "\t", "  ") + s[i:]
}

// parseRoutingHeader reads the leading --- ... --- YAML frontmatter of input using a strict
// stdlib-only subset parser. The closing --- must appear at column 0 so indented --- lines inside
// block scalars (such as special_input) cannot terminate frontmatter early. YAML keys are unquoted
// and normalized before checking forbidden repository fields.
func parseRoutingHeader(input string) (parsedRoutingHeader, error) {
	norm := strings.ReplaceAll(input, "\r\n", "\n")
	if !strings.HasPrefix(norm, "---\n") {
		return parsedRoutingHeader{}, bad("mapped project input must start with YAML frontmatter containing contract_ids or keys")
	}
	rest := norm[4:]
	lines := strings.Split(rest, "\n")
	end := -1
	for i, line := range lines {
		if line == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return parsedRoutingHeader{}, bad("mapped project input has unterminated YAML frontmatter")
	}
	fm := lines[:end]

	var contractIDs []string
	var genericKeys []string
	var treeLines []string
	var inlineTree []ContractTreeNode
	hasTree := false
	commitSHA := ""
	seenContractIDs := false
	seenGenericKeys := false

	section := "" // active top-level key for indented block values

	for _, rawLine := range fm {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := countLeadingSpaces(rawLine)
		if indent == 0 {
			section = ""
			key, val, ok := splitYAMLKeyVal(trimmed)
			if !ok {
				return parsedRoutingHeader{}, bad("invalid YAML frontmatter line %q", trimmed)
			}
			k := normalizeYAMLKey(key)
			v := strings.TrimSpace(val)
			if forbiddenMapHeaderKeys[k] {
				if v != "" && v != "null" && v != "~" && v != `""` && v != "''" && v != "[]" {
					return parsedRoutingHeader{}, bad("mapped project input must not specify repository field %q; repository selection is owned by Intelligence", k)
				}
				section = "forbidden:" + k
				continue
			}
			switch k {
			case "contract_ancestors", "key_ancestors":
				return parsedRoutingHeader{}, bad("unverified ancestor chains (%s) are not accepted; send contract_tree verified against project.treeSource", k)
			case "contract_ids":
				seenContractIDs = true
				if v != "" {
					items, err := parseYAMLFlowList(v)
					if err != nil {
						return parsedRoutingHeader{}, bad("invalid contract_ids in frontmatter: %s", err)
					}
					contractIDs = append(contractIDs, items...)
				} else {
					section = "contract_ids"
				}
			case "keys":
				seenGenericKeys = true
				if v != "" {
					items, err := parseYAMLFlowList(v)
					if err != nil {
						return parsedRoutingHeader{}, bad("invalid keys in frontmatter: %s", err)
					}
					genericKeys = append(genericKeys, items...)
				} else {
					section = "keys"
				}
			case "outline_commit_sha":
				commitSHA = normalizeYAMLKey(v)
			case "contract_tree":
				hasTree = true
				if v != "" {
					nodes, err := parseContractTreeJSON(v)
					if err != nil {
						return parsedRoutingHeader{}, bad("invalid contract_tree in frontmatter: %s", err)
					}
					inlineTree = nodes
				} else {
					section = "contract_tree"
				}
			default:
				section = "other"
			}
			continue
		}

		// Indented line under active section.
		if strings.HasPrefix(section, "forbidden:") {
			fk := strings.TrimPrefix(section, "forbidden:")
			return parsedRoutingHeader{}, bad("mapped project input must not specify repository field %q; repository selection is owned by Intelligence", fk)
		}
		switch section {
		case "contract_ids", "keys":
			if !strings.HasPrefix(trimmed, "-") {
				return parsedRoutingHeader{}, bad("expected list item under %s in frontmatter", section)
			}
			item := normalizeYAMLKey(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")))
			if item == "" {
				return parsedRoutingHeader{}, bad("empty item under %s in frontmatter", section)
			}
			if section == "contract_ids" {
				contractIDs = append(contractIDs, item)
			} else {
				genericKeys = append(genericKeys, item)
			}
		case "contract_tree":
			treeLines = append(treeLines, rawLine)
		default:
			// Ignore indented lines belonging to unrelated frontmatter fields (e.g., special_input, criteria_hashes).
		}
	}

	if seenContractIDs && seenGenericKeys {
		return parsedRoutingHeader{}, bad("mapped project input must not specify both contract_ids and keys")
	}
	ids := contractIDs
	if !seenContractIDs && seenGenericKeys {
		ids = genericKeys
	}
	if len(ids) == 0 {
		return parsedRoutingHeader{}, bad("mapped project input needs at least one entry in contract_ids or keys")
	}
	tree := inlineTree
	if len(treeLines) > 0 {
		parsed, err := parseContractTreeYAML(treeLines)
		if err != nil {
			return parsedRoutingHeader{}, bad("invalid contract_tree in frontmatter: %s", err)
		}
		tree = parsed
	}
	return parsedRoutingHeader{
		ids:       ids,
		tree:      tree,
		hasTree:   hasTree,
		commitSHA: commitSHA,
	}, nil
}

func parseContractTreeJSON(raw string) ([]ContractTreeNode, error) {
	var nodes []ContractTreeNode
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&nodes); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("unexpected trailing data")
	}
	return nodes, nil
}

// parseContractTreeYAML parses a standard yaml.v3 indented block sequence of {id, children} nodes.
func parseContractTreeYAML(lines []string) ([]ContractTreeNode, error) {
	type nodePtr struct {
		id       string
		children []*nodePtr
	}
	type frame struct {
		indent int
		node   *nodePtr
	}
	var roots []*nodePtr
	var stack []frame

	for _, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := countLeadingSpaces(rawLine)
		if strings.HasPrefix(trimmed, "-") {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
				stack = stack[:len(stack)-1]
			}
			np := &nodePtr{}
			if rest != "" {
				k, v, ok := splitYAMLKeyVal(rest)
				if !ok || normalizeYAMLKey(k) != "id" {
					return nil, fmt.Errorf("expected '- id: <uuid>', got %q", trimmed)
				}
				np.id = normalizeYAMLKey(v)
				if np.id == "" {
					return nil, fmt.Errorf("empty id in contract_tree")
				}
			}
			if len(stack) == 0 {
				roots = append(roots, np)
			} else {
				stack[len(stack)-1].node.children = append(stack[len(stack)-1].node.children, np)
			}
			stack = append(stack, frame{indent: indent, node: np})
			continue
		}
		if len(stack) == 0 || indent <= stack[len(stack)-1].indent {
			return nil, fmt.Errorf("unexpected indentation at %q", trimmed)
		}
		k, v, ok := splitYAMLKeyVal(trimmed)
		if !ok {
			return nil, fmt.Errorf("invalid contract_tree line %q", trimmed)
		}
		switch normalizeYAMLKey(k) {
		case "id":
			if stack[len(stack)-1].node.id != "" {
				return nil, fmt.Errorf("duplicate id in contract_tree node")
			}
			id := normalizeYAMLKey(v)
			if id == "" {
				return nil, fmt.Errorf("empty id in contract_tree")
			}
			stack[len(stack)-1].node.id = id
		case "children":
			vt := strings.TrimSpace(v)
			if vt != "" && vt != "[]" {
				sub, err := parseContractTreeJSON(vt)
				if err != nil {
					return nil, fmt.Errorf("invalid inline children: %w", err)
				}
				var convertIn func([]ContractTreeNode) []*nodePtr
				convertIn = func(ns []ContractTreeNode) []*nodePtr {
					out := make([]*nodePtr, len(ns))
					for i, n := range ns {
						out[i] = &nodePtr{id: normalizeYAMLKey(n.ID), children: convertIn(n.Children)}
					}
					return out
				}
				stack[len(stack)-1].node.children = append(stack[len(stack)-1].node.children, convertIn(sub)...)
			}
		default:
			return nil, fmt.Errorf("unsupported field %q in contract_tree", k)
		}
	}

	var freeze func([]*nodePtr) ([]ContractTreeNode, error)
	freeze = func(ptrs []*nodePtr) ([]ContractTreeNode, error) {
		out := make([]ContractTreeNode, len(ptrs))
		for i, p := range ptrs {
			if p.id == "" {
				return nil, fmt.Errorf("contract_tree node missing id")
			}
			kids, err := freeze(p.children)
			if err != nil {
				return nil, err
			}
			out[i] = ContractTreeNode{ID: p.id, Children: kids}
		}
		return out, nil
	}
	return freeze(roots)
}

func countLeadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	return n
}

func splitYAMLKeyVal(line string) (string, string, bool) {
	inSingle := false
	inDouble := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
		} else if c == '"' && !inSingle && (i == 0 || line[i-1] != '\\') {
			inDouble = !inDouble
		} else if c == ':' && !inSingle && !inDouble {
			if i+1 == len(line) || line[i+1] == ' ' || line[i+1] == '\t' {
				return line[:i], line[i+1:], true
			}
		}
	}
	return "", "", false
}

func parseYAMLFlowList(raw string) ([]string, error) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("expected YAML list")
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return nil, nil
	}
	var out []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			cur.WriteByte(c)
		} else if c == '"' && !inSingle && (i == 0 || inner[i-1] != '\\') {
			inDouble = !inDouble
			cur.WriteByte(c)
		} else if c == ',' && !inSingle && !inDouble {
			item := normalizeYAMLKey(cur.String())
			if item == "" {
				return nil, fmt.Errorf("empty list item")
			}
			out = append(out, item)
			cur.Reset()
		} else {
			cur.WriteByte(c)
		}
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote in list")
	}
	item := normalizeYAMLKey(cur.String())
	if item == "" {
		return nil, fmt.Errorf("empty list item")
	}
	out = append(out, item)
	return out, nil
}

func normalizeYAMLKey(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = strings.ReplaceAll(s[1:len(s)-1], `\"`, `"`)
		s = strings.ReplaceAll(s, `\\`, `\`)
	} else if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return strings.ToLower(strings.TrimSpace(s))
}
