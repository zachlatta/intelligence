package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// DisabledProviderError is a terminal launch refusal, not a transient host failure.
type DisabledProviderError struct{ Provider string }

func (e *DisabledProviderError) Error() string {
	return fmt.Sprintf("provider %s is disabled", e.Provider)
}

// DisabledProviders reads deployment plan policy; installed CLIs do not imply paid subscriptions.
func DisabledProviders(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT provider FROM provider_plan_settings WHERE NOT enabled ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ProviderRule overrides legacy model lists in saved instructions without rewriting history.
func ProviderRule(providers []string) string {
	if len(providers) == 0 {
		return ""
	}
	return "Subscription policy: these providers are disabled: " + strings.Join(providers, ", ") + ". Do not select, suggest or launch them, including through paseo or sub-agents. This overrides any model lists in saved workflow instructions. Pass this policy to every worker."
}

func checkCommandProviders(ctx context.Context, tx pgx.Tx, host, kind string, args json.RawMessage) (json.RawMessage, error) {
	disabled, err := DisabledProviders(ctx, tx)
	if err != nil || len(disabled) == 0 {
		return args, err
	}
	parsed, err := hostclient.ParseCommand(kind, args)
	if err != nil {
		return args, err
	}
	provider := ""
	var agent *hostclient.CreateAgentArgs
	switch a := parsed.(type) {
	case hostclient.CreateAgentArgs:
		agent = &a
	case hostclient.CreateWorkspaceArgs:
		agent = a.Agent
	case hostclient.ScheduleArgs:
		if a.Agent != nil {
			provider = a.Agent.Provider
		}
		if provider == "" && kind == hostclient.UpdateScheduleKind {
			if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT provider FROM schedules WHERE host=$1 AND id=$2),'')`, host, a.ScheduleID).Scan(&provider); err != nil {
				return args, err
			}
		}
	case hostclient.ScheduleIDArgs:
		if kind == hostclient.ResumeScheduleKind || kind == hostclient.RunScheduleKind {
			if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT provider FROM schedules WHERE host=$1 AND id=$2),'')`, host, a.ScheduleID).Scan(&provider); err != nil {
				return args, err
			}
		}
	}
	if agent != nil {
		provider = agent.Provider
	}
	for _, p := range disabled {
		if strings.EqualFold(strings.TrimSpace(provider), p) {
			return args, &DisabledProviderError{Provider: p}
		}
	}
	if agent != nil && agent.Labels["intelligence.run"] != "" {
		agent.Prompt += "\n\n" + ProviderRule(disabled)
		switch a := parsed.(type) {
		case hostclient.CreateAgentArgs:
			return json.Marshal(agent)
		case hostclient.CreateWorkspaceArgs:
			a.Agent = agent
			return json.Marshal(a)
		}
	}
	return args, nil
}
