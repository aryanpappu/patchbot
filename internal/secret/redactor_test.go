package secret_test

import (
	"encoding/base64"
	"strings"
	"sync"
	"testing"

	"github.com/aryanpappu/patchbot/internal/secret"
)

// Dummy tokens with the right shape. None of them is a real credential.
const (
	fakeGitHub    = "ghp_" + "0123456789abcdefghijABCDEFGHIJ012345"
	fakeGitHubPAT = "github_pat_" + "11ABCDEFG0abcdefghij_0123456789abcdefghij"
	fakeAnthropic = "sk-ant-" + "api03-abcdefghijklmnopqrstuvwxyz_0123456789-AA"
	fakeOpenAI    = "sk-" + "proj-abcdefghijklmnopqrstuvwxyz0123456789"
	fakeGoogle    = "AIza" + "SyA-abcdefghijklmnopqrstuvwxyz012345"
	//nolint:gosec // a made-up block that only has the shape of a key
	fakePEM = "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA0abc\ndef/ghi+jkl==\n-----END RSA PRIVATE KEY-----"
)

func TestRedactPatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text", "nothing to see here", "nothing to see here"},
		{"github classic", "token=" + fakeGitHub + " ok", "token=[redacted] ok"},
		{"github oauth", "gho_" + strings.Repeat("a", 36), "[redacted]"},
		{"github server", "ghs_" + strings.Repeat("B", 36), "[redacted]"},
		{"github user", "ghu_" + strings.Repeat("c", 36), "[redacted]"},
		{"github refresh", "ghr_" + strings.Repeat("9", 36), "[redacted]"},
		{"github fine-grained", "x " + fakeGitHubPAT + ".", "x [redacted]."},
		{"anthropic", "key " + fakeAnthropic, "key [redacted]"},
		{"openai", "OPENAI=" + fakeOpenAI + "\n", "OPENAI=[redacted]\n"},
		{"google", "?key=" + fakeGoogle + "&x=1", "?key=[redacted]&x=1"},
		{"bearer keeps the scheme", "Authorization: Bearer abcdefghijklmnop.qrstuv", "Authorization: Bearer [redacted]"},
		{"bearer lower case", "authorization: bearer abcdefghijklmnopqrstuv", "authorization: bearer [redacted]"},
		{"bearer with a known token", "Bearer " + fakeGitHub, "Bearer [redacted]"},
		{"pem", "a\n" + fakePEM + "\nb", "a\n[redacted]\nb"},
		{"pem without end", "a\n-----BEGIN PRIVATE KEY-----\nMIIabc\ndef", "a\n[redacted]"},
		{"two pem blocks", fakePEM + "\nmid\n" + fakePEM, "[redacted]\nmid\n[redacted]"},
		{"two tokens", fakeGitHub + " and " + fakeOpenAI, "[redacted] and [redacted]"},
		{"short prefix is not a token", "ghp_short and sk-short", "ghp_short and sk-short"},
		{"word ending in sk-", "task-list-for-the-very-long-running-job", "task-list-for-the-very-long-running-job"},
		{"token after an escaped newline", `{"out":"a\\n` + fakeGitHub + `\\nsk-` + strings.Repeat("Z", 48) + `"}`, `{"out":"a\\n[redacted]\\n[redacted]"}`},
		{"token after a url escape", "q=%20" + fakeGoogle + "%20Bearer%20x", "q=%20[redacted]%20Bearer%20x"},
		{"legacy openai", "sk-" + strings.Repeat("a1B2", 12), "[redacted]"},
		{"public key is kept", "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----", "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----"},
		{"mask is stable", "[redacted]", "[redacted]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A nil Redactor must still mask token shapes.
			for name, r := range map[string]*secret.Redactor{"nil": nil, "empty": secret.NewRedactor()} {
				if got := r.Redact(tt.in); got != tt.want {
					t.Errorf("%s Redactor: Redact(%q) = %q, want %q", name, tt.in, got, tt.want)
				}
			}
			once := secret.NewRedactor().Redact(tt.in)
			if twice := secret.NewRedactor().Redact(once); twice != once {
				t.Errorf("Redact is not idempotent: %q then %q", once, twice)
			}
		})
	}
}

func TestRedactValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values []string
		in     string
		want   string
	}{
		{"one value", []string{"hunter2-hunter2"}, "pw=hunter2-hunter2;", "pw=[redacted];"},
		{"repeated", []string{"hunter2-hunter2"}, "hunter2-hunter2 hunter2-hunter2", "[redacted] [redacted]"},
		{"short value ignored", []string{"abc1234"}, "abc1234", "abc1234"},
		{"empty value ignored", []string{""}, "text", "text"},
		{"exactly eight chars", []string{"abcd1234"}, "xabcd1234x", "x[redacted]x"},
		{"one value contains the other", []string{"abcdefgh", "abcdefghijkl"}, "<abcdefghijkl>", "<[redacted]>"},
		{"overlapping values", []string{"abcdefgh12", "gh12345678"}, "<abcdefgh12345678>", "<[redacted]>"},
		{"self-overlapping value", []string{"abababab"}, "<ababababab>", "<[redacted]>"},
		{"adjacent values", []string{"aaaaaaaa1", "bbbbbbbb2"}, "aaaaaaaa1bbbbbbbb2", "[redacted]"},
		{"value next to a token", []string{"hunter2-hunter2"}, "hunter2-hunter2" + fakeGitHub, "[redacted]"},
		{"value with regexp characters", []string{"a.*b[c]{2}$^(d)"}, "x a.*b[c]{2}$^(d) y", "x [redacted] y"},
		{"multi-line value", []string{"line1\nline2\nline3"}, "a line1\nline2\nline3 b", "a [redacted] b"},
		{"unicode value", []string{"пароль-пароль"}, "é пароль-пароль é", "é [redacted] é"},
		{
			"base64 form",
			[]string{"hunter2-hunter2"},
			"Basic " + base64.StdEncoding.EncodeToString([]byte("hunter2-hunter2")),
			"Basic [redacted]",
		},
		{
			"raw url base64 form",
			[]string{"hunter2-hunter?>"},
			base64.RawURLEncoding.EncodeToString([]byte("hunter2-hunter?>")),
			"[redacted]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := secret.NewRedactor(tt.values...)
			if got := r.Redact(tt.in); got != tt.want {
				t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if got := string(r.RedactBytes([]byte(tt.in))); got != tt.want {
				t.Errorf("RedactBytes(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedactorAddSecrets(t *testing.T) {
	t.Parallel()
	r := secret.NewRedactor()
	r.AddSecrets(secret.Secret("first-secret-value"), secret.Secret(""), secret.Secret("tiny"))
	if got, want := r.Redact("a first-secret-value tiny"), "a [redacted] tiny"; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}

func TestRedactorConcurrentUse(t *testing.T) {
	t.Parallel()
	r := secret.NewRedactor("initial-secret-value")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 200 {
				if i%2 == 0 {
					r.Add(strings.Repeat("abcdefgh"[i:i+1], 8+j%5))
					continue
				}
				if got := r.Redact("x initial-secret-value y"); got != "x [redacted] y" {
					t.Errorf("Redact = %q", got)
					return
				}
			}
		})
	}
	wg.Wait()
}
