package fleet

import (
	"context"
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

// CheckProvider refuses provider if it is one of disabled (from DisabledProviders).
func CheckProvider(disabled []string, provider string) error {
	for _, p := range disabled {
		if strings.EqualFold(strings.TrimSpace(provider), p) {
			return &DisabledProviderError{Provider: p}
		}
	}
	return nil
}

// checkCommandProvider refuses a command that would start or resume work on a disabled provider.
func checkCommandProvider(ctx context.Context, tx pgx.Tx, host, kind string, parsed any) error {
	disabled, err := DisabledProviders(ctx, tx)
	if err != nil || len(disabled) == 0 {
		return err
	}
	provider, scheduleID := "", ""
	switch a := parsed.(type) {
	case hostclient.CreateAgentArgs:
		provider = a.Provider
	case hostclient.CreateWorkspaceArgs:
		if a.Agent != nil {
			provider = a.Agent.Provider
		}
	case hostclient.ScheduleArgs:
		if a.Agent != nil {
			provider = a.Agent.Provider
		}
		if provider == "" && kind == hostclient.UpdateScheduleKind {
			scheduleID = a.ScheduleID
		}
	case hostclient.ScheduleIDArgs:
		if kind == hostclient.ResumeScheduleKind || kind == hostclient.RunScheduleKind {
			scheduleID = a.ScheduleID
		}
	}
	if scheduleID != "" {
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT provider FROM schedules WHERE host=$1 AND id=$2),'')`, host, scheduleID).Scan(&provider); err != nil {
			return err
		}
	}
	return CheckProvider(disabled, provider)
}
