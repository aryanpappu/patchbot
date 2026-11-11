//go:build unix

package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/testpath"
)

func TestCloneUsesConfiguredTrustedGitAndChildPath(t *testing.T) {
	bin := testpath.Dir(t)
	marker := filepath.Join(t.TempDir(), "selected")
	script := "#!/bin/sh\nprintf '%s' \"$PATH\" > '" + marker + "'\necho 'git version 1.0.0'\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	} // #nosec G306 -- executable fixture.
	_, err := Clone(t.Context(), CloneOptions{Dir: filepath.Join(t.TempDir(), "checkout"), Branch: "main", TestOnlyLocalRemote: t.TempDir(), TrustedPath: []string{bin}})
	if err == nil || !strings.Contains(err.Error(), MinimumGitVersion) {
		t.Fatalf("custom old git was not checked: %v", err)
	}
	got, readErr := os.ReadFile(marker) // #nosec G304 -- marker belongs to this test.
	if readErr != nil || string(got) != bin {
		t.Fatalf("git used a different executable/PATH: %q %v", got, readErr)
	}
}
