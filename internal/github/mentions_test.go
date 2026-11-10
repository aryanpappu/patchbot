package github

import "testing"

func TestNeutralizeMentions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"user", "Thanks @alice!", "Thanks @\u200dalice!"},
		{"team", "@org/team please review", "@\u200dorg/team please review"},
		{"multiple", "(@alice), @Bob-1 and @org/team", "(@\u200dalice), @\u200dBob-1 and @\u200dorg/team"},
		{"emails", "a@b.com first.last+tag@example.org", "a@b.com first.last+tag@example.org"},
		{"punctuation", "-@alice .@bob /@org/team", "-@\u200dalice .@\u200dbob /@\u200dorg/team"},
		{"inline code", "`@alice` and @bob", "`@alice` and @\u200dbob"},
		{"double backticks", "`` `@alice` `` @bob", "`` `@alice` `` @\u200dbob"},
		{"fenced code", "```go\n@alice\n```\n@bob", "```go\n@alice\n```\n@\u200dbob"},
		{"tilde fence", "~~~text\n@org/team\n~~~\n@bob", "~~~text\n@org/team\n~~~\n@\u200dbob"},
		{"longer fence", "````\n```\n@alice\n````\n@bob", "````\n```\n@alice\n````\n@\u200dbob"},
		{"invalid closing fence", "```\n@alice\n```text\n@bob\n```\n@carol", "```\n@alice\n```text\n@bob\n```\n@\u200dcarol"},
		{"indented text is not a fence", "    ```\n@alice", "    ```\n@\u200dalice"},
		{"indented fence", "  ```\n@alice\n  ```\n@bob", "  ```\n@alice\n  ```\n@\u200dbob"},
		{"unicode", "こんにちは @alice", "こんにちは @\u200dalice"},
		{"not a mention", "@ @- @/", "@ @- @/"},
		{"already neutralized", "@\u200dalice", "@\u200dalice"},
		{"escaped mention", "\\@alice", "\\@\u200dalice"},
		{"unclosed inline", "a ` character @alice", "a ` character @\u200dalice"},
		{"escaped backtick", "\\` @alice", "\\` @\u200dalice"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := NeutralizeMentions(tt.in)
			if got != tt.want {
				t.Fatalf("NeutralizeMentions(%q) = %q; want %q", tt.in, got, tt.want)
			}
			if twice := NeutralizeMentions(got); twice != got {
				t.Errorf("not idempotent: %q", twice)
			}
		})
	}
}
