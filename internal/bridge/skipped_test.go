package bridge

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mailme/internal/gmail"
)

func waitForSyncState(t *testing.T, b *Bridge, ready func(*state) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		ok := ready(b.state)
		b.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("sync did not reach the expected state")
		}
		time.Sleep(time.Millisecond)
	}
}

func startSyncLoop(t *testing.T, b *Bridge, interval time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.RunSync(ctx, interval) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("sync/retry loops did not stop on cancellation")
		}
	})
}

func waitForSyncSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("sync worker did not reach the expected request")
	}
}

func TestLegacySkipped404IsDroppedOnceAndPersists(t *testing.T) {
	f := largeFake(2)
	dir := t.TempDir()
	b := openAPI(t, dir, f, Options{})
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	const missing, healthy = "m00000", "m00001"
	uid, validity := b.state.Folders["INBOX"].Members[healthy], b.state.Folders["INBOX"].Validity
	if err := b.disk.putRaw(missing, []byte(fixture)); err != nil {
		t.Fatal(err)
	}
	// Persist the v0.0.1 shape: an already-missing ID remains in Skipped even
	// though its previous metadata and body are still present in this cache.
	if err := b.update(func(st *state) {
		st.Skipped[missing] = (&gmail.APIError{Status: 404, Method: "messages.get", ID: missing, Reasons: []string{"notFound"}}).Error()
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	delete(f.messages, missing)
	f.mu.Unlock()
	var attempts atomic.Int32
	h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if id == missing {
			attempts.Add(1)
		}
		return f.Message(ctx, id, format)
	}, changes: func(context.Context, string) ([]string, string, error) { return nil, "20", nil }}
	b = openAPI(t, dir, h, Options{})
	t.Cleanup(func() {
		if b != nil {
			b.Close()
		}
	})
	if err := b.retrySkipped(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 1 || b.state.Skipped[missing] != "" || b.state.Messages[missing] != nil || b.state.Folders["INBOX"].Members[missing] != 0 {
		t.Fatal("404 did not remove the skipped ID and cached membership after one attempt")
	}
	if raw, err := b.disk.raw(missing); err != nil || len(raw) != 0 {
		t.Fatal("404 left a cached body:", err)
	}
	if b.state.History != "10" || b.state.Folders["INBOX"].Members[healthy] != uid || b.state.Folders["INBOX"].Validity != validity {
		t.Fatal("skipped pass changed history or a healthy message's IMAP identity")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = nil
	b = openAPI(t, dir, h, Options{})
	if err := b.retrySkipped(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 1 || b.state.Messages[missing] != nil || b.state.Skipped[missing] != "" || b.state.History != "20" {
		t.Fatal("removed 404 ID was retried or resurrected after reopening the data directory")
	}
}

func TestSkippedRetryPassClearsLegacy404Backlog(t *testing.T) {
	var attempts atomic.Int32
	h := &hookedAPI{API: largeFake(0), get: func(context.Context, string, string) (*gmail.Message, error) {
		attempts.Add(1)
		return nil, &gmail.APIError{Status: 404, Reasons: []string{"notFound"}}
	}}
	b := openAPI(t, t.TempDir(), h, Options{})
	defer b.Close()
	if err := b.update(func(st *state) {
		st.History = "10"
		for n := 0; n < 1084; n++ {
			st.Skipped[fmt.Sprintf("old-%04d", n)] = "Gmail API returned HTTP 404"
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.retrySkipped(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 1084 || len(b.state.Skipped) != 0 || b.state.History != "10" {
		t.Fatal("first pass did not clear the entire legacy 404 set without changing history")
	}
	if err := b.retrySkipped(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 1084 {
		t.Fatal("a subsequent pass retried terminal 404 IDs")
	}
}

func TestSnapshot404CheckpointDoesNotRefetchOnResume(t *testing.T) {
	f := largeFake(3)
	var missingAttempts atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if id == "m00001" {
			missingAttempts.Add(1)
			return nil, &gmail.APIError{Status: 404}
		}
		if id == "m00002" && fail.Load() {
			return nil, &gmail.APIError{Status: 401}
		}
		return f.Message(ctx, id, format)
	}}
	dir := t.TempDir()
	b := openAPI(t, dir, h, Options{Workers: 1, BatchSize: 1})
	if err := b.Sync(context.Background()); err == nil {
		t.Fatal("expected interrupted backfill")
	}
	if b.state.Pending == nil || !b.state.Pending.Done["m00001"] || len(b.state.Skipped) != 0 || b.state.History != "" {
		t.Fatal("404 completion was not checkpointed without advancing history")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	fail.Store(false)
	b = openAPI(t, dir, h, Options{Workers: 1, BatchSize: 1})
	defer b.Close()
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if missingAttempts.Load() != 1 || b.state.Pending != nil || b.state.History != "10" || len(b.state.Messages) != 2 || len(b.state.Skipped) != 0 {
		t.Fatal("snapshot resume retried a checkpointed 404 or lost healthy batches")
	}
}

func TestSkippedPassKeepsCommittedRemovalOnFailure(t *testing.T) {
	f := largeFake(2)
	var missingAttempts atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if id == "m00000" {
			missingAttempts.Add(1)
			return nil, &gmail.APIError{Status: 404}
		}
		if fail.Load() {
			return nil, &gmail.APIError{Status: 401}
		}
		return f.Message(ctx, id, format)
	}}
	dir := t.TempDir()
	b := openAPI(t, dir, h, Options{BatchSize: 1})
	if err := b.update(func(st *state) {
		st.History = "10"
		st.Skipped["m00000"], st.Skipped["m00001"] = "old 404", "transient error"
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.retrySkipped(context.Background()); err == nil {
		t.Fatal("expected systemic retry failure")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	fail.Store(false)
	b = openAPI(t, dir, h, Options{})
	defer b.Close()
	if b.state.History != "10" || b.state.Skipped["m00000"] != "" || b.state.Skipped["m00001"] == "" {
		t.Fatal("failed retry pass lost a committed deletion or changed history")
	}
	if err := b.retrySkipped(context.Background()); err != nil {
		t.Fatal(err)
	}
	if missingAttempts.Load() != 1 || len(b.state.Skipped) != 0 || b.state.Messages["m00001"] == nil || b.state.History != "10" {
		t.Fatal("retry did not resume remaining IDs without refetching the removed ID")
	}
}

func TestCloseCancelsAndWaitsForSkippedRead(t *testing.T) {
	started := make(chan struct{})
	h := &hookedAPI{API: largeFake(0), get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	b := openAPI(t, t.TempDir(), h, Options{})
	if err := b.update(func(st *state) { st.History = "10"; st.Skipped["old"] = "transient error" }); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- b.retrySkipped(context.Background()) }()
	waitForSyncSignal(t, started)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close left a skipped read using the cache")
	}
}

func TestHistoryPollCompletesDuringLongSkippedPass(t *testing.T) {
	f := largeFake(1)
	var arrived atomic.Bool
	var retryRequests atomic.Int32
	var retryReturned atomic.Bool
	started := make(chan struct{})
	h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if strings.HasPrefix(id, "old-") {
			if retryRequests.Add(1) == 1 {
				close(started)
			}
			defer retryReturned.Store(true)
			<-ctx.Done() // a whole retry pass cannot finish while this read is pending
			return nil, ctx.Err()
		}
		return f.Message(ctx, id, format)
	}, changes: func(ctx context.Context, cursor string) ([]string, string, error) {
		if arrived.Load() && cursor == "10" {
			return []string{"m00000"}, "20", nil
		}
		return nil, cursor, nil
	}}
	b := openAPI(t, t.TempDir(), h, Options{Workers: 6, SkippedRetryInterval: 20 * time.Millisecond})
	t.Cleanup(func() { b.Close() })
	if err := b.update(func(st *state) {
		st.History = "10"
		for n := 0; n < 8; n++ {
			st.Skipped[fmt.Sprintf("old-%02d", n)] = "transient metadata error"
		}
	}); err != nil {
		t.Fatal(err)
	}
	startSyncLoop(t, b, 10*time.Millisecond)
	waitForSyncSignal(t, started)
	arrived.Store(true)
	waitForSyncState(t, b, func(st *state) bool { return st.History == "20" && st.Messages["m00000"] != nil })
	if retryReturned.Load() || retryRequests.Load() != 1 {
		t.Fatal("history waited for the retry pass or retries filled multiple API slots")
	}
}

func TestRestartHonorsSkippedIntervalBeforeFirstPass(t *testing.T) {
	f := largeFake(1)
	var oldRequests atomic.Int32
	h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if strings.HasPrefix(id, "old-") {
			oldRequests.Add(1)
			return nil, &gmail.APIError{Status: 404}
		}
		return f.Message(ctx, id, format)
	}, changes: func(ctx context.Context, cursor string) ([]string, string, error) {
		if cursor == "10" {
			return []string{"m00000"}, "20", nil
		}
		return nil, cursor, nil
	}}
	dir := t.TempDir()
	b := openAPI(t, dir, h, Options{})
	if err := b.update(func(st *state) {
		st.History = "10"
		for n := 0; n < 1084; n++ {
			st.Skipped[fmt.Sprintf("old-%04d", n)] = "Gmail API returned HTTP 404"
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = openAPI(t, dir, h, Options{SkippedRetryInterval: 6 * time.Hour})
	t.Cleanup(func() { b.Close() })
	startSyncLoop(t, b, 10*time.Millisecond)
	waitForSyncState(t, b, func(st *state) bool { return st.History == "20" && st.Messages["m00000"] != nil })
	if oldRequests.Load() != 0 {
		t.Fatalf("restart forced %d skipped reads before the 6h interval", oldRequests.Load())
	}
}

func TestSnapshotResumeDoesNotForceCompletedSkippedReads(t *testing.T) {
	f := largeFake(1)
	var oldRequests atomic.Int32
	h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if strings.HasPrefix(id, "old-") {
			oldRequests.Add(1)
			return nil, &gmail.APIError{Status: 404}
		}
		return f.Message(ctx, id, format)
	}, changes: func(context.Context, string) ([]string, string, error) { return []string{"m00000"}, "20", nil }}
	dir := t.TempDir()
	b := openAPI(t, dir, h, Options{})
	if err := b.update(func(st *state) {
		// Simulate a crash after the last old-version batch, before promotion.
		st.Pending = &snapshot{History: "10", Listed: true, Done: map[string]bool{}, Touched: map[string]bool{}}
		for n := 0; n < 1084; n++ {
			id := fmt.Sprintf("old-%04d", n)
			st.Pending.IDs = append(st.Pending.IDs, id)
			st.Pending.Done[id] = true
			st.Skipped[id] = "Gmail API returned HTTP 404"
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = openAPI(t, dir, h, Options{SkippedRetryInterval: 6 * time.Hour})
	defer b.Close()
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.state.Pending != nil || b.state.History != "10" || oldRequests.Load() != 0 || len(b.state.Skipped) != 1084 {
		t.Fatal("resuming a completed snapshot forced a skipped pass or lost pending repair IDs")
	}
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.state.History != "20" || b.state.Messages["m00000"] == nil || oldRequests.Load() != 0 {
		t.Fatal("history after snapshot resume waited behind checkpointed skipped IDs")
	}
}

func TestSkippedResultCannotOverwriteHistory(t *testing.T) {
	for _, test := range []struct {
		name                 string
		historyFinishesFirst bool
		historyError         error
	}{
		{name: "history in flight"},
		{name: "history committed", historyFinishesFirst: true},
		{name: "newer identical transient error", historyFinishesFirst: true, historyError: &gmail.APIError{Status: 400, State: "INVALID_ARGUMENT"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := largeFake(1)
			retryStarted, historyStarted := make(chan struct{}), make(chan struct{})
			retryRelease, historyRelease := make(chan struct{}), make(chan struct{})
			var requests atomic.Int32
			h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
				if requests.Add(1) == 1 {
					close(retryStarted)
					select {
					case <-retryRelease:
						return nil, &gmail.APIError{Status: 404}
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				close(historyStarted)
				select {
				case <-historyRelease:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if test.historyError != nil {
					return nil, test.historyError
				}
				return &gmail.Message{ID: id, InternalDate: "1000", LabelIDs: []string{"INBOX", "STARRED"}}, nil
			}, changes: func(context.Context, string) ([]string, string, error) { return []string{"m00000"}, "20", nil }}
			b := openAPI(t, t.TempDir(), h, Options{})
			if err := b.update(func(st *state) {
				st.History = "10"
				st.Skipped["m00000"] = "old metadata error"
				if test.historyError != nil {
					st.Skipped["m00000"] = test.historyError.Error()
				}
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			retryDone, historyDone := make(chan struct{}), make(chan struct{})
			var retryErr, historyErr error
			go func() { retryErr = b.retrySkipped(ctx); close(retryDone) }()
			defer func() { cancel(); <-retryDone; b.Close() }()
			waitForSyncSignal(t, retryStarted)
			go func() { historyErr = b.Sync(ctx); close(historyDone) }()
			defer func() { cancel(); <-historyDone }()
			waitForSyncSignal(t, historyStarted)
			if test.historyFinishesFirst {
				close(historyRelease)
				waitForSyncSignal(t, historyDone)
				close(retryRelease)
			} else {
				close(retryRelease)
				waitForSyncSignal(t, retryDone)
				close(historyRelease)
			}
			waitForSyncSignal(t, retryDone)
			waitForSyncSignal(t, historyDone)
			if retryErr != nil || historyErr != nil {
				t.Fatalf("retry=%v history=%v", retryErr, historyErr)
			}
			if test.historyError != nil {
				if b.state.History != "20" || b.state.Skipped["m00000"] != test.historyError.Error() {
					t.Fatal("older skipped 404 erased a newer transient history failure")
				}
				return
			}
			if b.state.History != "20" || b.state.Skipped["m00000"] != "" || b.state.Messages["m00000"] == nil || !contains(b.state.Messages["m00000"].Message.LabelIDs, "STARRED") {
				t.Fatal("older skipped 404 deleted or deferred a newer history result")
			}
		})
	}
}
