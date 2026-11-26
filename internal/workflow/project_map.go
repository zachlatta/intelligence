package workflow

import (
	"fmt"
	"path"
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

type parsedRoutingHeader struct {
	ids       []string
	ancestors map[string][]string
}

// ResolveProjectRoute deterministically resolves a mapped workflow's single target repository from
// structured YAML frontmatter at the top of input. It fails closed with a Refusal when input lacks
// routing keys, supplies forbidden repository fields, spoofs an ancestor chain, includes an unmapped
// key, or spans more than one repository.
func ResolveProjectRoute(routes []ProjectRoute, input string) (ProjectRoute, error) {
	if err := validateProjectRoutes(routes); err != nil {
		return ProjectRoute{}, bad("%s", err)
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

	var chosen *ProjectRoute
	for _, id := range hdr.ids {
		chain := []string{id}
		if anc, ok := hdr.ancestors[id]; ok {
			if len(anc) == 0 || anc[0] != id {
				return ProjectRoute{}, bad("routing ancestor chain for %q must begin with %q", id, id)
			}
			chain = anc
		}
		var matched *ProjectRoute
		for _, candidate := range chain {
			if rt, ok := index[candidate]; ok {
				cp := rt
				matched = &cp
				break
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

// parseRoutingHeader reads the leading --- ... --- YAML frontmatter of input using a strict
// stdlib-only subset parser for top-level scalar/sequence keys and one-level ancestor maps.
func parseRoutingHeader(input string) (parsedRoutingHeader, error) {
	norm := strings.ReplaceAll(input, "\r\n", "\n")
	if !strings.HasPrefix(norm, "---\n") {
		return parsedRoutingHeader{}, bad("mapped project input must start with YAML frontmatter containing contract_ids or keys")
	}
	rest := norm[4:]
	lines := strings.Split(rest, "\n")
	end := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
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
	contractAncestors := map[string][]string{}
	keyAncestors := map[string][]string{}
	seenContractIDs := false
	seenGenericKeys := false

	section := ""      // active top-level key for indented block values
	ancTarget := ""    // active map key inside contract_ancestors / key_ancestors
	sectionIndent := 0 // indent of child key inside ancestors map

	for _, rawLine := range fm {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := countLeadingSpaces(rawLine)
		if indent == 0 {
			ancTarget = ""
			section = ""
			key, val, ok := splitYAMLKeyVal(trimmed)
			if !ok {
				return parsedRoutingHeader{}, bad("invalid YAML frontmatter line %q", trimmed)
			}
			k := strings.ToLower(strings.TrimSpace(key))
			v := strings.TrimSpace(val)
			if forbiddenMapHeaderKeys[k] {
				if v != "" && v != "null" && v != "~" && v != `""` && v != "''" && v != "[]" {
					return parsedRoutingHeader{}, bad("mapped project input must not specify repository field %q; repository selection is owned by Intelligence", k)
				}
				section = "forbidden:" + k
				continue
			}
			switch k {
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
			case "contract_ancestors":
				if v != "" && v != "{}" {
					return parsedRoutingHeader{}, bad("contract_ancestors must be a YAML mapping of contract id to ancestor list")
				}
				section = "contract_ancestors"
			case "key_ancestors":
				if v != "" && v != "{}" {
					return parsedRoutingHeader{}, bad("key_ancestors must be a YAML mapping of key to ancestor list")
				}
				section = "key_ancestors"
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
		case "contract_ancestors", "key_ancestors":
			dst := contractAncestors
			if section == "key_ancestors" {
				dst = keyAncestors
			}
			if strings.HasPrefix(trimmed, "-") {
				if ancTarget == "" || indent < sectionIndent {
					return parsedRoutingHeader{}, bad("unexpected list item under %s in frontmatter", section)
				}
				item := normalizeYAMLKey(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")))
				if item == "" {
					return parsedRoutingHeader{}, bad("empty ancestor under %s[%q]", section, ancTarget)
				}
				dst[ancTarget] = append(dst[ancTarget], item)
				continue
			}
			key, val, ok := splitYAMLKeyVal(trimmed)
			if !ok {
				return parsedRoutingHeader{}, bad("invalid entry under %s in frontmatter: %q", section, trimmed)
			}
			ancTarget = normalizeYAMLKey(key)
			sectionIndent = indent
			if ancTarget == "" {
				return parsedRoutingHeader{}, bad("empty key under %s in frontmatter", section)
			}
			v := strings.TrimSpace(val)
			if v != "" {
				items, err := parseYAMLFlowList(v)
				if err != nil {
					return parsedRoutingHeader{}, bad("invalid ancestor list for %q: %s", ancTarget, err)
				}
				dst[ancTarget] = items
				ancTarget = ""
			} else if _, exists := dst[ancTarget]; !exists {
				dst[ancTarget] = []string{}
			}
		default:
			// Ignore indented lines belonging to unrelated frontmatter fields (e.g., criteria_hashes).
		}
	}

	if seenContractIDs && seenGenericKeys {
		return parsedRoutingHeader{}, bad("mapped project input must not specify both contract_ids and keys")
	}
	ids := contractIDs
	ancestors := contractAncestors
	if !seenContractIDs && seenGenericKeys {
		ids = genericKeys
		ancestors = keyAncestors
	}
	if len(ids) == 0 {
		return parsedRoutingHeader{}, bad("mapped project input needs at least one entry in contract_ids or keys")
	}
	return parsedRoutingHeader{ids: ids, ancestors: ancestors}, nil
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
