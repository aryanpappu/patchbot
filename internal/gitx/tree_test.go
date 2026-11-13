package gitx

import (
	"context"
	"path/filepath"
	"testing"
)

// TreeID identifies the complete intended worktree, including untracked files,
// and equals the committed tree. Any later byte change yields another ID.
func TestTreeIDBindsCommittedContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := gitFixture(t)
	r := f.clone(t, 1)
	clean, err := r.TreeID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head, err := r.HeadTree(ctx); err != nil || head != clean {
		t.Fatalf("clean tree %q, head %q, %v", clean, head, err)
	}
	if err := r.CreateBranch(ctx, "patchbot/issue-3"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.Dir(), "new.txt"), "new\n")
	reviewed, err := r.TreeID(ctx)
	if err != nil || reviewed == clean {
		t.Fatalf("untracked file not in tree: %q, %v", reviewed, err)
	}
	if again, err := r.TreeID(ctx); err != nil || again != reviewed {
		t.Fatalf("tree is not stable: %q, %v", again, err)
	}
	write(t, filepath.Join(r.Dir(), "new.txt"), "changed\n")
	if changed, err := r.TreeID(ctx); err != nil || changed == reviewed {
		t.Fatalf("content change not detected: %q, %v", changed, err)
	}
	write(t, filepath.Join(r.Dir(), "new.txt"), "new\n")
	if _, err := r.CommitAll(ctx, "add", Author{}); err != nil {
		t.Fatal(err)
	}
	if head, err := r.HeadTree(ctx); err != nil || head != reviewed {
		t.Fatalf("committed tree %q differs from reviewed %q: %v", head, reviewed, err)
	}
}
