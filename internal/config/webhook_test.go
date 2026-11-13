package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebhookConfigSecretAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := `[webhook]
enabled = true
secret_env = "CUSTOM_HOOK_SECRET"
path = "/custom-hook"
trigger_labels = { "custom" = "refine" }
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(Options{Path: path, LookupEnv: func(name string) (string, bool) { return "custom-webhook-secret-value", name == "CUSTOM_HOOK_SECRET" }})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Webhook.Enabled || cfg.Webhook.Path != "/custom-hook" || cfg.Webhook.TriggerLabels["custom"] != "refine" || len(cfg.Webhook.TriggerLabels) != 1 || !cfg.Webhook.Secret.IsSet() {
		t.Fatalf("webhook config not loaded")
	}
	var shown strings.Builder
	if err := cfg.WriteEffective(&shown); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(shown.String(), cfg.Webhook.Secret.Reveal()) || !strings.Contains(shown.String(), "CUSTOM_HOOK_SECRET") {
		t.Fatal("secret display boundary")
	}
	cfg.Webhook.SecretEnv = "bad env"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "webhook.secret_env") {
		t.Fatalf("missing validation: %v", err)
	}
}
