package config_test

import (
	"strings"
	"testing"
)

const accessBase = validBase + `
[server.auth]
mode = "cloudflare_access"
team_domain = "crimson-scene-dba4.cloudflareaccess.com"
audience = "0123456789abcdef0123456789abcdef"
allowed_emails = ["Owner@Example.com"]
`

func TestServerAuthValidation(t *testing.T) {
	t.Parallel()
	if err := loadString(t, validBase, nil).Validate(); err != nil {
		t.Fatalf("default token mode: %v", err)
	}
	if got := loadString(t, validBase, nil).Server.Auth.Mode; got != "token" {
		t.Fatalf("default mode %q", got)
	}
	if err := loadString(t, accessBase, nil).Validate(); err != nil {
		t.Fatalf("valid access config: %v", err)
	}
	// A non-loopback bind is allowed because Access authenticates requests.
	if err := loadString(t, accessBase+"\n[server]\naddr = \"0.0.0.0:8080\"\n", nil).Validate(); err != nil {
		t.Fatalf("non-loopback access bind: %v", err)
	}
	replace := func(from, to string) string { return strings.Replace(accessBase, from, to, 1) }
	tests := []struct{ name, file, want string }{
		{"bad mode", validBase + "[server.auth]\nmode = \"magic\"", "server.auth.mode"},
		{"scheme in domain", replace(`"crimson-scene-dba4.cloudflareaccess.com"`, `"https://crimson-scene-dba4.cloudflareaccess.com"`), "server.auth.team_domain"},
		{"other domain", replace("cloudflareaccess.com", "example.com"), "server.auth.team_domain"},
		{"path in domain", replace(`.cloudflareaccess.com"`, `.cloudflareaccess.com/x"`), "server.auth.team_domain"},
		{"missing audience", replace(`audience = "0123456789abcdef0123456789abcdef"`, `audience = ""`), "server.auth.audience"},
		{"short audience", replace("0123456789abcdef0123456789abcdef", "abc"), "server.auth.audience"},
		{"no emails", replace(`["Owner@Example.com"]`, `[]`), "server.auth.allowed_emails: list at least one"},
		{"bad email", replace("Owner@Example.com", "not-an-email"), "server.auth.allowed_emails"},
		{"wildcard email", replace("Owner@Example.com", "*@example.com"), "server.auth.allowed_emails"},
		{"access fields in token mode", validBase + "[server.auth]\nmode = \"token\"\naudience = \"x\"", "apply only to mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := loadString(t, tt.file, nil).Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
