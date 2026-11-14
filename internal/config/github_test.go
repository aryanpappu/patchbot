package config_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/config"
)

func TestGitHubAuthValidation(t *testing.T) {
	t.Parallel()
	ok := []string{
		"",
		"[github]\nauth = \"pat\"\n",
		"[github]\nauth = \"app\"\napp_id = 123\nprivate_key_file = \"/srv/patchbot/github-app.pem\"\n",
		"[github]\nauth = \"app\"\napp_id = 123\nprivate_key_file = \"/srv/patchbot/github-app.pem\"\ninstallation_id = 9\n",
		// app_id 0 is setup mode: Connect GitHub fills it from the database.
		"[github]\nauth = \"app\"\nprivate_key_file = \"/srv/patchbot/github-app.pem\"\nsetup_public_url = \"https://patchbot.example.org\"\nsetup_app_name = \"my patchbot\"\n",
		"[github]\nsetup_public_url = \"http://localhost:8080\"\n",
	}
	for _, extra := range ok {
		if err := loadString(t, validBase+extra, nil).Validate(); err != nil {
			t.Fatalf("%q: %v", extra, err)
		}
	}
	bad := map[string]string{
		"[github]\nauth = \"oauth\"\n": "github.auth",
		"[github]\nauth = \"app\"\napp_id = -1\nprivate_key_file = \"/srv/patchbot/k.pem\"\n":             "github.app_id",
		"[github]\nsetup_public_url = \"http://patchbot.example.org\"\n":                                  "github.setup_public_url",
		"[github]\nsetup_public_url = \"https://patchbot.example.org/x?y=1\"\n":                           "github.setup_public_url",
		"[github]\nsetup_public_url = \"https://u:p@patchbot.example.org\"\n":                             "github.setup_public_url",
		"[github]\nsetup_app_name = \"bad<name>\"\n":                                                      "github.setup_app_name",
		"[github]\nauth = \"app\"\napp_id = 1\n":                                                          "github.private_key_file",
		"[github]\nauth = \"app\"\napp_id = 1\nprivate_key_file = \"key.pem\"\n":                          "github.private_key_file",
		"[github]\nauth = \"app\"\napp_id = 1\nprivate_key_file = \"/srv/x.pem\"\ninstallation_id = -1\n": "github.installation_id",
	}
	for extra, want := range bad {
		err := loadString(t, validBase+extra, nil).Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: got %v, want %s", extra, err, want)
		}
	}
	// A key inside the data directory is refused.
	inside := validBase + "[data]\ndir = \"/srv/patchbot/data\"\n[github]\nauth = \"app\"\napp_id = 1\nprivate_key_file = \"/srv/patchbot/data/workspaces/k.pem\"\n"
	if err := loadString(t, inside, nil).Validate(); err == nil || !strings.Contains(err.Error(), "data directory") {
		t.Fatalf("key in data dir: %v", err)
	}
}

func TestGitHubPrivateKeyFileIsNotASecretKeyButOtherKeysAre(t *testing.T) {
	t.Parallel()
	cfg := loadString(t, validBase+"[github]\nauth = \"app\"\napp_id = 5\nprivate_key_file = \"/srv/patchbot/github-app.pem\"\n", nil)
	var out bytes.Buffer
	if err := cfg.WriteEffective(&out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"[github]", `auth = "app"`, "# file", `private_key_file = "/srv/patchbot/github-app.pem"`, "installation_id = 0", "# default"} {
		if !strings.Contains(text, want) {
			t.Fatalf("config show lacks %q:\n%s", want, text)
		}
	}
	e := newEnv(t)
	e.local(t, validBase+"[github]\nprivate_key = \"-----BEGIN RSA PRIVATE KEY-----\"\n")
	if _, err := config.Load(e.options()); err == nil || !strings.Contains(err.Error(), "looks like a secret") {
		t.Fatalf("inline key accepted: %v", err)
	}
}
