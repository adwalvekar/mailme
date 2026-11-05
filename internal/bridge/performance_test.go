package bridge

import (
	"context"
	"testing"
	"time"

	"mailme/internal/gmail"
)

func TestCheckpointIntervalPublishesPartialBatch(t *testing.T) {
	f := largeFake(3)
	blocked := make(chan struct{})
	release := make(chan struct{})
	h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if id == "m00002" {
			close(blocked)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return f.Message(ctx, id, format)
	}}
	b := openAPI(t, t.TempDir(), h, Options{Workers: 1, BatchSize: 250, CheckpointInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var syncErr error
	go func() { syncErr = b.Sync(ctx); close(done) }()
	defer func() { cancel(); <-done; b.Close() }()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("metadata worker did not reach blocked request")
	}
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		published := len(b.state.Messages)
		b.mu.Unlock()
		if published == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("partial batch waited for blocked request instead of checkpoint interval")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	<-done
	if syncErr != nil {
		t.Fatal(syncErr)
	}
}

func TestPollingCadenceIncludesSyncDuration(t *testing.T) {
	starts := make(chan time.Time, 4)
	h := &hookedAPI{API: largeFake(0), changes: func(ctx context.Context, cursor string) ([]string, string, error) {
		starts <- time.Now()
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil, cursor, nil
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}}
	b := openAPI(t, t.TempDir(), h, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.RunSync(ctx, 500*time.Millisecond); close(done) }()
	defer func() { cancel(); <-done; b.Close() }()
	nextStart := func() time.Time {
		select {
		case start := <-starts:
			return start
		case <-time.After(3 * time.Second):
			t.Fatal("poll did not start")
			return time.Time{}
		}
	}
	first, second := nextStart(), nextStart()
	if delta := second.Sub(first); delta < 450*time.Millisecond || delta > 650*time.Millisecond {
		t.Fatalf("poll interval added sync duration: %s", delta)
	}
}

// Measures the idle path with a real 16,650-message index and bbolt cache.
// No Gmail requests, subprocesses, or simulated API latency are involved.
func BenchmarkIdleSync(b *testing.B) {
	h := &hookedAPI{API: largeFake(16650), changes: func(context.Context, string) ([]string, string, error) {
		return nil, "10", nil
	}}
	bridge, err := Open(b.TempDir(), testEmail, testPassword, h)
	if err != nil {
		b.Fatal(err)
	}
	defer bridge.Close()
	if err := bridge.Sync(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if err := bridge.Sync(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
