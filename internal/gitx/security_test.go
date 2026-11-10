package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestGitVersionGate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		out     string
		version [3]int
		allowed bool
	}{
		{"git version 2.31.0", [3]int{2, 31, 0}, false},
		{"git version 2.50.0", [3]int{2, 50, 0}, false},
		{"git version 1.99.99", [3]int{1, 99, 99}, false},
		{"git version 2.50.1", [3]int{2, 50, 1}, true},
		{"git version 2.50.1.windows.1\n", [3]int{2, 50, 1}, true},
		{"git version 2.50.1 (Apple Git-157)\n", [3]int{2, 50, 1}, true},
		{"git version 2.39.5 (Apple Git-154)", [3]int{2, 39, 5}, false},
		{"git version 2.50.10", [3]int{2, 50, 10}, true},
		{"git version 2.100.0", [3]int{2, 100, 0}, true},
		{"git version 3.0.0", [3]int{3, 0, 0}, true},
	} {
		t.Run(strings.TrimSpace(test.out), func(t *testing.T) {
			version, err := parseGitVersion(test.out)
			if err != nil || version != test.version {
				t.Fatalf("parse = %v, %v; want %v", version, err, test.version)
			}
			err = checkGitVersion(test.out)
			if (err == nil) != test.allowed {
				t.Fatalf("version gate = %v; allowed %v", err, test.allowed)
			}
			if err != nil && (!strings.Contains(err.Error(), strings.TrimSpace(test.out)) ||
				!strings.Contains(err.Error(), MinimumGitVersion)) {
				t.Fatalf("error must name found and required versions: %v", err)
			}
		})
	}
	for _, malformed := range []string{"git version 2.50", "git version 2.50.1oops", "git version 2.50.-1", "not git"} {
		if err := checkGitVersion(malformed); err == nil {
			t.Errorf("accepted malformed version %q", malformed)
		}
	}
}

func TestDiffSizeLimitsBeforeDiff(t *testing.T) {
	t.Parallel()
	f := gitFixture(t)
	for _, kind := range []string{"untracked", "worktree", "staged-blob", "committed-blob", "deleted-base-blob", "total"} {
		t.Run(kind, func(t *testing.T) {
			r := f.clone(t, 0)
			path := "large.txt"
			if kind == "worktree" {
				path = "second.txt"
			}
			if kind == "total" {
				for i := range 11 {
					writeSizedFile(t, filepath.Join(r.Dir(), fmt.Sprintf("large-%d", i)), MaxDiffFileSize)
				}
			} else {
				writeSizedFile(t, filepath.Join(r.Dir(), path), 6<<20)
			}
			switch kind {
			case "staged-blob":
				f.git(t, r.Dir(), "add", "--", path)
				// The index still contains a huge blob even when the worktree is tiny.
				write(t, filepath.Join(r.Dir(), path), "small\n")
			case "committed-blob", "deleted-base-blob":
				f.git(t, r.Dir(), "add", "--", path)
				f.git(t, r.Dir(), "commit", "--no-verify", "-m", "huge blob")
				if kind == "deleted-base-blob" {
					r.base = strings.TrimSpace(f.git(t, r.Dir(), "rev-parse", "HEAD"))
					f.git(t, r.Dir(), "rm", "--", path)
					f.git(t, r.Dir(), "commit", "--no-verify", "-m", "delete huge blob")
				}
			}
			var diffCalls, stageCalls int
			r.observeCommand = func(args []string) {
				switch args[0] {
				case "diff":
					diffCalls++
				case "add":
					stageCalls++
				}
			}
			for _, operation := range []string{"Diff", "DiffStat"} {
				var err error
				if operation == "Diff" {
					_, err = r.Diff(context.Background())
				} else {
					_, err = r.DiffStat(context.Background())
				}
				var tooLarge *ErrDiffTooLarge
				if !errors.As(err, &tooLarge) {
					t.Fatalf("%s error = %v; want *ErrDiffTooLarge", operation, err)
				}
				if kind == "total" {
					if tooLarge.Path != "" || tooLarge.Limit != MaxDiffTotalSize {
						t.Fatalf("aggregate error = %+v", tooLarge)
					}
				} else if tooLarge.Path != path || tooLarge.Size != 6<<20 || tooLarge.Limit != MaxDiffFileSize {
					t.Fatalf("file size error = %+v", tooLarge)
				}
			}
			if diffCalls != 0 || stageCalls != 0 {
				t.Fatalf("oversized input invoked diff %d times and add %d times", diffCalls, stageCalls)
			}
		})
	}
}

func writeSizedFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600) // #nosec G304 -- test-owned temporary fixture path.
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(size)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("size fixture: %v, %v", err, closeErr)
	}
}

func TestDiffRejectsExpandedSnapshot(t *testing.T) {
	t.Parallel()
	f := gitFixture(t)
	r := f.clone(t, 0)
	write(t, filepath.Join(r.Dir(), ".gitattributes"), "encoded.txt working-tree-encoding=ISO-8859-1\n")
	// Three MiB of Latin-1 expands to six MiB of UTF-8 when Git stages it.
	write(t, filepath.Join(r.Dir(), "encoded.txt"), strings.Repeat("\xe9", 3<<20))
	var diffCalls int
	r.observeCommand = func(args []string) {
		if args[0] == "diff" {
			diffCalls++
		}
	}
	_, diffErr := r.Diff(context.Background())
	_, statErr := r.DiffStat(context.Background())
	for _, err := range []error{diffErr, statErr} {
		var tooLarge *ErrDiffTooLarge
		if !errors.As(err, &tooLarge) || tooLarge.Path != "encoded.txt" || tooLarge.Size != 6<<20 {
			t.Fatalf("expanded snapshot error = %v; want a six-MiB *ErrDiffTooLarge", err)
		}
	}
	if diffCalls != 0 {
		t.Fatalf("oversized snapshot invoked diff %d times", diffCalls)
	}
}

func TestDiffBinaryClassification(t *testing.T) {
	t.Parallel()
	f := gitFixture(t)
	for _, kind := range []string{"nul", "attributes", "threshold", "mixed"} {
		t.Run(kind, func(t *testing.T) {
			r := f.clone(t, 0)
			data := "\x00binary payload\n"
			switch kind {
			case "attributes":
				data = "Git attributes classify this text as binary\n"
				mustMkdir(t, filepath.Join(r.Dir(), ".git", "info"))
				write(t, filepath.Join(r.Dir(), ".git", "info", "attributes"), "binary.dat -diff\n")
			case "threshold":
				// The single-file limit itself is allowed; Git treats it as binary.
				data = strings.Repeat("a", int(MaxDiffFileSize))
			}
			write(t, filepath.Join(r.Dir(), "binary.dat"), data)
			wantStat := DiffStat{Files: 1}
			var wantPatchPaths []string
			if kind == "mixed" {
				write(t, filepath.Join(r.Dir(), "text.txt"), "text change\n")
				wantStat = DiffStat{Files: 2, Insertions: 1}
				wantPatchPaths = []string{"text.txt"}
			}
			var patchPaths []string
			r.observeCommand = func(args []string) {
				if args[0] != "diff" {
					return
				}
				for _, flag := range []string{"--no-ext-diff", "--no-textconv"} {
					if !slices.Contains(args, flag) {
						t.Errorf("diff missing %s", flag)
					}
				}
				if !slices.Contains(args, "--numstat") {
					patchPaths = append(patchPaths, args[slices.Index(args, "--")+1:]...)
				}
			}
			diff, err := r.Diff(context.Background())
			if err != nil || !strings.Contains(diff, `Binary file "binary.dat" changed`) {
				t.Fatalf("binary diff = %q, %v", diff, err)
			}
			if !reflect.DeepEqual(patchPaths, wantPatchPaths) {
				t.Fatalf("patch paths = %q; want %q", patchPaths, wantPatchPaths)
			}
			if kind == "mixed" && !strings.Contains(diff, "+text change") {
				t.Fatalf("missing text patch: %q", diff)
			}
			if stat, err := r.DiffStat(context.Background()); err != nil || stat != wantStat {
				t.Fatalf("binary stat = %+v, %v; want %+v", stat, err, wantStat)
			}
			cmd, err := r.command(context.Background(), false, "diff")
			if err != nil || !slices.Contains(cmd.Args, "core.bigFileThreshold=1m") {
				t.Fatalf("missing big-file threshold: %v", err)
			}
		})
	}
}
