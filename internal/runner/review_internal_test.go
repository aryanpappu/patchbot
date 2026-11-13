package runner

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestReserveForReview(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := agent.Limits{MaxTotalTokens: 1000, MaxCostMicros: 2000, Deadline: now.Add(100 * time.Second)}
	got := reserveForReview(lim, 0.15, now)
	if got.MaxTotalTokens != 850 || got.MaxCostMicros != 1700 || !got.Deadline.Equal(now.Add(85*time.Second)) {
		t.Fatalf("reserved limits: %+v", got)
	}
	if same := reserveForReview(lim, 0, now); same != lim {
		t.Fatalf("zero reserve changed limits: %+v", same)
	}
}

func TestFrozenReviewValidation(t *testing.T) {
	for raw, ok := range map[string]bool{
		``:                  true,
		`{"enabled":false}`: true,
		`{"enabled":true,"provider":"p","model":"m","max_repairs":1,"max_turns":15,"reserve_fraction":0.15}`: true,
		`{"enabled":true,"provider":"p","model":"m","max_repairs":2,"max_turns":15}`:                         false,
		`{"enabled":true,"provider":"","model":"m","max_repairs":1,"max_turns":15}`:                          false,
		`{"enabled":true,"provider":"p","model":"m","max_repairs":1,"max_turns":0}`:                          false,
		`{"enabled":true,"provider":"p","model":"m","max_repairs":1,"max_turns":1,"reserve_fraction":0.9}`:   false,
		`not json`: false,
	} {
		run := store.Run{}
		if raw != "" {
			run.ReviewJSON = json.RawMessage(raw)
		}
		if _, err := frozenReview(run); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
}
