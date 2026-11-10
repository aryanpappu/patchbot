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
		if got := EligibleCommand(comment, approvers, check, bot); got != want {
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
			if EligibleCommand(tt.comment, tt.approvers, tt.check, tt.bot) {
				t.Fatal("invalid identity or metadata accepted")
			}
		})
	}
	// Equal instants with different timezone representations are unedited.
	comment.UpdatedAt = now.In(time.FixedZone("NPT", 20700))
	if !EligibleCommand(comment, []string{"ALICE"}, check, "patchbot") {
		t.Fatal("equal timestamp instants rejected")
	}
}
