package workflow

import (
	"strings"
	"testing"
)

func TestCLIAskSecretFlagParsing(t *testing.T) {
	t.Parallel()
	m, run, err := parseCLI([]string{
		"ask", "--run", "run-1", "--note", "-",
		"--secret", "Stripe API key: Read billing invoices",
		"--secret", `{"name":"DB password","purpose":"Prod migration","username":"migrator"}`,
	}, strings.NewReader("Please provide these secrets."))
	if err != nil {
		t.Fatalf("parseCLI: %v", err)
	}
	if run != "run-1" || m.Move != "ask" || len(m.Secrets) != 2 {
		t.Fatalf("unexpected move: %+v", m)
	}
	if m.Secrets[0].Name != "Stripe API key" || m.Secrets[0].Purpose != "Read billing invoices" {
		t.Fatalf("secret 0: %+v", m.Secrets[0])
	}
	if m.Secrets[1].Name != "DB password" || m.Secrets[1].Purpose != "Prod migration" || m.Secrets[1].Username != "migrator" {
		t.Fatalf("secret 1: %+v", m.Secrets[1])
	}
}

func TestSecretRequestValidation(t *testing.T) {
	t.Parallel()
	// Rejected on non-ask moves.
	if _, err := validateSecretRequests("next", []SecretRequest{{Name: "API key"}}); err == nil {
		t.Fatal("accepted secrets on non-ask move")
	}
	// Duplicate names rejected case-insensitively.
	if _, err := validateSecretRequests("ask", []SecretRequest{{Name: "API Key"}, {Name: "api key"}}); err == nil {
		t.Fatal("accepted duplicate secret names")
	}
	// Multiline name rejected.
	if _, err := validateSecretRequests("ask", []SecretRequest{{Name: "bad\nname"}}); err == nil {
		t.Fatal("accepted multiline secret name")
	}
	// Too many secrets rejected.
	many := make([]SecretRequest, maxSecretsPerAsk+1)
	for i := range many {
		many[i] = SecretRequest{Name: "key-" + string(rune('a'+i))}
	}
	if _, err := validateSecretRequests("ask", many); err == nil {
		t.Fatal("accepted more than maxSecretsPerAsk")
	}
	// Unknown JSON field rejected.
	if _, err := parseSecretFlag(`{"name":"Key","value":"nope"}`); err == nil {
		t.Fatal("accepted unknown field in --secret JSON")
	}
}

func TestOrchestratorPromptMentionsSecretAskFlag(t *testing.T) {
	t.Parallel()
	prompt, err := OrchestratorPrompt("Demo", Starting, Vars{Run: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `--secret "<name>[: <purpose>]"`) || !strings.Contains(prompt, "Vaultwarden") {
		t.Fatalf("orchestrator prompt missing --secret instructions")
	}
}
