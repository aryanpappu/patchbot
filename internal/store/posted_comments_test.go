package store

import "testing"

func TestPostedComments(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := t.Context()
	if posted, err := s.IsPostedComment(ctx, 77); err != nil || posted {
		t.Fatalf("unrecorded comment = %v, %v", posted, err)
	}
	c := PostedComment{CommentID: 77, RunID: 1, Repo: "owner/repo", IssueNumber: 12, Kind: "spec"}
	for range 2 { // A repeated record of the same ID is a no-op.
		if err := s.RecordPostedComment(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if posted, err := s.IsPostedComment(ctx, 77); err != nil || !posted {
		t.Fatalf("recorded comment = %v, %v", posted, err)
	}
	for _, bad := range []PostedComment{
		{RunID: 1, Repo: "owner/repo", IssueNumber: 12, Kind: "spec"},
		{CommentID: 78, Repo: "owner/repo", IssueNumber: 12, Kind: "spec"},
		{CommentID: 78, RunID: 1, IssueNumber: 12, Kind: "spec"},
		{CommentID: 78, RunID: 1, Repo: "owner/repo", Kind: "spec"},
		{CommentID: 78, RunID: 1, Repo: "owner/repo", IssueNumber: 12},
	} {
		if err := s.RecordPostedComment(ctx, bad); err == nil {
			t.Fatalf("invalid record accepted: %+v", bad)
		}
	}
}
