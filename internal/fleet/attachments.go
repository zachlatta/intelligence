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
	var text *string
	var images *[]attachment.Image
	switch a := parsed.(type) {
	case hostclient.CreateAgentArgs:
		parsed = &a
		text = &a.Prompt
		images = &a.Attachments
	case hostclient.CreateWorkspaceArgs:
		if a.Agent == nil {
			return args, nil
		}
		parsed = &a
		text = &a.Agent.Prompt
		images = &a.Agent.Attachments
	case hostclient.AgentArgs:
		if kind != hostclient.SendMessageKind {
			return args, nil
		}
		parsed = &a
		text = &a.Text
		images = &a.Attachments
	default:
		return args, nil
	}
	if len(*images) > 0 {
		return nil, fmt.Errorf("upload images through /api/attachments and use their references")
	}
	if !attachment.Reference.MatchString(*text) {
		return args, nil
	}
	seen := map[string]bool{}
	total := 0
	for _, m := range attachment.Reference.FindAllStringSubmatch(*text, -1) {
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
		*images = append(*images, img)
		*text = strings.ReplaceAll(*text, m[0], "Image (open this file; pass its path to any sub-agents): "+img.Path())
	}
	return json.Marshal(parsed)
}
