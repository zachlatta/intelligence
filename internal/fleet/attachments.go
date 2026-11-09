package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/attachment"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// Resolve only images owned by the destination host's account. Workflow input retains the
// references, so both the project finder and the later orchestrator receive delivery copies.
func resolveImages(ctx context.Context, tx pgx.Tx, host, kind string, args json.RawMessage) (json.RawMessage, error) {
	parsed, err := hostclient.ParseCommand(kind, args)
	if err != nil {
		return nil, err
	}
	var agent *hostclient.CreateAgentArgs
	switch a := parsed.(type) {
	case hostclient.CreateAgentArgs:
		agent = &a
	case hostclient.CreateWorkspaceArgs:
		agent = a.Agent
	default:
		return args, nil
	}
	if agent == nil {
		return args, nil
	}
	if len(agent.Attachments) > 0 {
		return nil, fmt.Errorf("upload images through /api/attachments and use their references")
	}
	if !attachment.Reference.MatchString(agent.Prompt) {
		return args, nil
	}
	seen := map[string]bool{}
	total := 0
	for _, m := range attachment.Reference.FindAllStringSubmatch(agent.Prompt, -1) {
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		var img attachment.Image
		img.ID = id
		err := tx.QueryRow(ctx, `SELECT a.media_type,a.data FROM attachments a JOIN host_keys h ON h.account_id=a.account_id WHERE a.id=$1 AND h.host=$2`, id, host).Scan(&img.MediaType, &img.Data)
		if err != nil {
			return nil, fmt.Errorf("image attachment is unavailable for this account")
		}
		total += len(img.Data)
		if len(seen) > attachment.MaxCount || total > attachment.MaxTotal {
			return nil, fmt.Errorf("attach at most 8 images, 8 MiB total")
		}
		agent.Attachments = append(agent.Attachments, img)
		agent.Prompt = strings.ReplaceAll(agent.Prompt, m[0], "Image (open this file; pass its path to any sub-agents): "+img.Path())
	}
	switch a := parsed.(type) {
	case hostclient.CreateAgentArgs:
		return json.Marshal(agent)
	case hostclient.CreateWorkspaceArgs:
		a.Agent = agent
		return json.Marshal(a)
	}
	return args, nil
}
