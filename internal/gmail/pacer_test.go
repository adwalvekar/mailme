package gmail

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Costs are the public Gmail API contract as of May 2026. In particular, raw
// body downloads must consume three times the metadata quota in the default.
func TestQuotaCosts(t *testing.T) {
	for _, test := range []struct {
		method, format  string
		current, legacy int
	}{
		{"messages.get", "metadata", 20, 5},
		{"messages.get", "raw", 60, 5},
		{"messages.get", "full", 60, 5},
		{"messages.get", "html", 60, 5},
		{"messages.list", "", 5, 5},
		{"messages.modify", "", 5, 5},
		{"messages.trash", "", 20, 5},
		{"messages.untrash", "", 5, 5},
		{"messages.send", "", 100, 100},
		{"getProfile", "", 1, 1},
		{"labels.list", "", 1, 1},
		{"history.list", "", 2, 2},
	} {
		for _, profile := range []string{"", "current", "legacy"} {
			want := test.current
			if profile == "legacy" {
				want = test.legacy
			}
			if got := Cost(test.method, test.format, profile); got != want {
				t.Errorf("%s/%s profile=%q: got %d want %d", test.method, test.format, profile, got, want)
			}
		}
	}
}

func TestDefaultPacerSharesQuotaAndCancels(t *testing.T) {
	var p Pacer
	ctx := context.Background()
	start := time.Now()
	if err := p.Wait(ctx, Cost("messages.get", "metadata", ""), ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(ctx, Cost("messages.get", "raw", ""), ""); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 240*time.Millisecond {
		t.Fatal("body request did not share the metadata quota budget")
	}
	cancelCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if err := p.Wait(cancelCtx, 20, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("quota wait ignored cancellation: %v", err)
	}
}
