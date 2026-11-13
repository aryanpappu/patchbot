package github

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestParseCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want Command
		ok   bool
	}{
		{name: "approve", body: "/approve", want: Command{Kind: CommandApprove}, ok: true},
		{name: "mixed case and whitespace", body: "\n \t\n   /ApPrOvE \r\n", want: Command{Kind: CommandApprove}, ok: true},
		{name: "reject", body: "/reject", want: Command{Kind: CommandReject}, ok: true},
		{name: "revise multiline", body: "\n /ReViSe use a queue\r\nkeep retries\n\nexplain backoff\n", want: Command{Kind: CommandRevise, Notes: "use a queue\nkeep retries\n\nexplain backoff"}, ok: true},
		{name: "revise notes next line", body: "/revise\nnotes", want: Command{Kind: CommandRevise, Notes: "notes"}, ok: true},
		{name: "answer", body: "/answer yes\nwith tests", want: Command{Kind: CommandAnswer, Notes: "yes\nwith tests"}, ok: true},
		{name: "tabs", body: "/revise\tadd tests", want: Command{Kind: CommandRevise, Notes: "add tests"}, ok: true},
		{name: "reject reason next line", body: "/reject\nno longer needed", want: Command{Kind: CommandReject, Notes: "no longer needed"}, ok: true},
		{name: "quote", body: " > /approve\n/approve"},
		{name: "nested quote", body: ">> /approve"},
		{name: "fenced", body: "```text\n/approve\n```"},
		{name: "tilde fenced", body: "~~~\n/approve\n~~~"},
		{name: "inline code", body: "`/approve`"},
		{name: "second line", body: "looks good\n/approve"},
		{name: "second command", body: "/unknown\n/approve"},
		{name: "unknown", body: "/ship"},
		{name: "suffix", body: "/approve-now"},
		{name: "approve inline arguments", body: "/approve please"},
		{name: "reject inline arguments", body: "/reject please"},
		{name: "empty revise", body: "/revise\n \n"},
		{name: "empty answer", body: "/answer "},
		{name: "empty", body: "\n\t"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseCommand(Comment{Body: tt.body})
			if ok != tt.ok || !cmp.Equal(got, tt.want) {
				t.Fatalf("ParseCommand() = %#v, %v; want %#v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestEligibleCommand(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	// Exhaust all combinations of the four authorization predicates.
	for flags := 0; flags < 16; flags++ {
		allowed, collaborator, unedited, self := flags&1 != 0, flags&2 != 0, flags&4 != 0, flags&8 != 0
		comment := Comment{Author: "Alice", CreatedAt: now, UpdatedAt: now}
		approvers := []string{"other"}
		if allowed {
			approvers = []string{"ALICE"}
		}
		if !unedited {
			comment.UpdatedAt = now.Add(time.Second)
		}
		bot := "patchbot"
		if self {
			bot = "aLiCe"
		}
		calls := 0
		check := func(login string) bool {
			calls++
			if login != "Alice" {
				t.Errorf("collaborator login = %q", login)
			}
			return collaborator
		}
		want := allowed && collaborator && unedited && !self
		if got := EligibleCommand(comment, approvers, check, bot, nil); got != want {
			t.Errorf("flags %04b: got %v, want %v", flags, got, want)
		}
		if calls > 0 && (!allowed || !unedited || self) {
			t.Errorf("flags %04b: unauthorized comment triggered membership lookup", flags)
		}
	}
	comment := Comment{Author: "alice", CreatedAt: now, UpdatedAt: now}
	check := func(string) bool { return true }
	for _, tt := range []struct {
		name      string
		comment   Comment
		approvers []string
		check     func(string) bool
		bot       string
	}{
		{"empty approvers", comment, nil, check, "patchbot"},
		{"unknown bot", comment, []string{"alice"}, check, ""},
		{"nil callback", comment, []string{"alice"}, nil, "patchbot"},
		{"missing timestamps", Comment{Author: "alice"}, []string{"alice"}, check, "patchbot"},
		{"missing author", Comment{CreatedAt: now, UpdatedAt: now}, []string{""}, check, "patchbot"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if EligibleCommand(tt.comment, tt.approvers, tt.check, tt.bot, func(int64) bool { return false }) {
				t.Fatal("invalid identity or metadata accepted")
			}
		})
	}
	// Equal instants with different timezone representations are unedited.
	comment.UpdatedAt = now.In(time.FixedZone("NPT", 20700))
	if !EligibleCommand(comment, []string{"ALICE"}, check, "patchbot", nil) {
		t.Fatal("equal timestamp instants rejected")
	}
}

func TestEligibleCommandSharedIdentity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	owner := Comment{ID: 501, Author: "aryanpappu", Body: "/approve", CreatedAt: now, UpdatedAt: now}
	member := func(string) bool { return true }
	posted := map[int64]bool{900: true}
	record := func(id int64) bool { return posted[id] }
	approvers := []string{"AryanPappu"}

	if !EligibleCommand(owner, approvers, member, "aryanpappu", record) {
		t.Fatal("owner's own /approve rejected when the token owner is an approver")
	}
	for _, tt := range []struct {
		name      string
		mutate    func(*Comment)
		approvers []string
		record    func(int64) bool
		member    func(string) bool
	}{
		{"recorded patchbot comment", func(c *Comment) { c.ID = 900 }, approvers, record, member},
		{"spec comment starting with /approve", func(c *Comment) {
			c.Body = "/approve\n\nmodel-written spec\n<!-- patchbot:spec run=1 v=1 sha=abc -->"
		}, approvers, record, member},
		{"marker without ID record", func(c *Comment) { c.Body = "/approve\n" + Marker("comment") }, approvers, record, member},
		{"marker variant", func(c *Comment) { c.Body = "/approve\n<!--   PatchBot:x -->" }, approvers, record, member},
		{"edited owner comment", func(c *Comment) { c.UpdatedAt = now.Add(time.Minute) }, approvers, record, member},
		{"bot not an approver", func(*Comment) {}, []string{"someone-else"}, record, member},
		{"no approvers", func(*Comment) {}, nil, record, member},
		{"record lookup failed (true)", func(*Comment) {}, approvers, func(int64) bool { return true }, member},
		{"no record callback", func(*Comment) {}, approvers, nil, member},
		{"missing ID", func(c *Comment) { c.ID = 0 }, approvers, record, member},
		{"not a collaborator", func(*Comment) {}, approvers, record, func(string) bool { return false }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			comment := owner
			tt.mutate(&comment)
			if EligibleCommand(comment, tt.approvers, tt.member, "aryanpappu", tt.record) {
				t.Fatal("shared-identity comment accepted")
			}
		})
	}
	// A distinct bot account keeps the old rules: the bot (not an approver)
	// never counts and a human approver is judged exactly as before.
	alice := Comment{ID: 8, Author: "alice", Body: "/approve", CreatedAt: now, UpdatedAt: now}
	if !EligibleCommand(alice, []string{"alice"}, member, "patchbot", nil) {
		t.Fatal("human approver rejected with a distinct bot account")
	}
	if EligibleCommand(Comment{ID: 9, Author: "patchbot", Body: "/approve", CreatedAt: now, UpdatedAt: now},
		[]string{"alice"}, member, "patchbot", func(int64) bool { return false }) {
		t.Fatal("distinct bot account comment accepted")
	}
}

func TestHasPatchbotMarker(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]bool{
		"/approve":                    false,
		"/approve <!-- other -->":     false,
		"x\n" + Marker("failure"):     true,
		"<!--patchbot:spec run=1 -->": true,
		"<!-- PATCHBOT -->":           true,
	} {
		if got := HasPatchbotMarker(body); got != want {
			t.Errorf("HasPatchbotMarker(%q) = %v, want %v", body, got, want)
		}
	}
}
