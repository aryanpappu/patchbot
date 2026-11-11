package cli_test

import (
	"strings"
	"testing"
)

func TestServeUsageAndBindRefusal(t *testing.T) {
	for _, args := range [][]string{{"serve", "extra"}, {"serve", "--unknown"}, {"serve", "--addr"}} {
		code, _, stderr := runApp(t, validConfig, nil, args...)
		if code != 2 || !strings.Contains(stderr, "Usage: patchbot serve") {
			t.Fatalf("usage: %d %s", code, stderr)
		}
	}
	code, _, stderr := runApp(t, validConfig, nil, "serve", "--addr", "0.0.0.0:8080")
	if code != 1 || !strings.Contains(stderr, "requires PATCHBOT_UI_TOKEN") {
		t.Fatalf("bind refusal: %d %s", code, stderr)
	}
	code, _, stderr = runApp(t, validConfig, map[string]string{"PATCHBOT_ADDR": "0.0.0.0:8080"}, "serve")
	if code != 1 || !strings.Contains(stderr, "requires PATCHBOT_UI_TOKEN") {
		t.Fatalf("configured bind refusal: %d %s", code, stderr)
	}
}
