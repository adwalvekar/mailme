package bridge

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	bolt "go.etcd.io/bbolt"
	"mailme/internal/gmail"
)

type hookedAPI struct {
	API
	get     func(context.Context, string, string) (*gmail.Message, error)
	changes func(context.Context, string) ([]string, string, error)
	inbox   []string
	labels  func(context.Context) ([]gmail.Label, error)
}

func (h *hookedAPI) Labels(ctx context.Context) ([]gmail.Label, error) {
	if h.labels != nil {
		return h.labels(ctx)
	}
	return h.API.Labels(ctx)
}

func (h *hookedAPI) Message(ctx context.Context, id, format string) (*gmail.Message, error) {
	if h.get != nil {
		return h.get(ctx, id, format)
	}
	return h.API.Message(ctx, id, format)
}
func (h *hookedAPI) IDs(ctx context.Context) ([]string, error) {
	ids, err := h.API.IDs(ctx)
	sort.Strings(ids)
	return ids, err
}
func (h *hookedAPI) InboxIDs(context.Context) ([]string, error) { return h.inbox, nil }
func (h *hookedAPI) Changes(ctx context.Context, cursor string) ([]string, string, error) {
	if h.changes != nil {
		return h.changes(ctx, cursor)
	}
	return h.API.Changes(ctx, cursor)
}

func largeFake(n int) *fakeAPI {
	f := newFake()
	f.messages = make(map[string]gmail.Message)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("m%05d", i)
		f.messages[id] = gmail.Message{ID: id, LabelIDs: []string{"INBOX"}, InternalDate: fmt.Sprint(1000 + i)}
	}
	return f
}
func openAPI(t *testing.T, dir string, api API, options Options) *Bridge {
	t.Helper()
	b, err := Open(dir, testEmail, testPassword, api, options)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBadMessagesAreRetriedWithoutLosingSnapshot(t *testing.T) {
	for name, failure := range map[string]error{
		"invalid argument":    &gmail.APIError{Status: 400, Reasons: []string{"invalidArgument"}},
		"failed precondition": &gmail.APIError{Status: 400, State: "FAILED_PRECONDITION"},
		"malformed":           &gmail.MalformedMessageError{ID: "m00001"},
	} {
		t.Run(name, func(t *testing.T) {
			f := largeFake(3)
			f.failID, f.errorGet = "m00001", failure
			h := &hookedAPI{API: f, changes: func(context.Context, string) ([]string, string, error) { return nil, "20", nil }}
			b := openAPI(t, t.TempDir(), h, Options{Workers: 1})
			defer b.Close()
			if err := b.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(b.state.Messages) != 2 || len(b.state.Skipped) != 1 || b.state.Pending != nil || b.state.History != "10" {
				t.Fatal("bad ID prevented cursor promotion or lost healthy messages")
			}
			f.errorGet = nil
			if err := b.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(b.state.Messages) != 2 || len(b.state.Skipped) != 1 {
				t.Fatal("history poll ran an inline skipped pass")
			}
			if err := b.retrySkipped(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(b.state.Messages) != 3 || len(b.state.Skipped) != 0 || b.state.History != "20" {
				t.Fatal("independent skipped retry lost the ID or changed the history checkpoint")
			}
		})
	}
}

func TestIdleSyncDoesNotRewriteIndexAndPersistsCursor(t *testing.T) {
	f := largeFake(100)
	var labelCalls int
	h := &hookedAPI{API: f, labels: func(ctx context.Context) ([]gmail.Label, error) {
		labelCalls++
		return f.Labels(ctx)
	}}
	dir := t.TempDir()
	b := openAPI(t, dir, h, Options{})
	t.Cleanup(func() {
		if b != nil {
			b.Close()
		}
	})
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	index := func() string {
		var data string
		if err := b.disk.db.View(func(tx *bolt.Tx) error {
			data = string(tx.Bucket([]byte("state")).Get([]byte("index")))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := index()
	h.changes = func(context.Context, string) ([]string, string, error) { return nil, "20", nil }
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if index() != before || b.state.History != "20" || labelCalls != 1 {
		t.Fatal("idle sync rewrote index, missed cursor, or refetched labels")
	}
	// Even a due, unchanged label response must avoid a full index commit.
	b.lastLabels = time.Time{}
	previousState := b.state
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.state != previousState || index() != before || labelCalls != 2 {
		t.Fatal("unchanged poll committed or failed to refresh due labels")
	}
	uid := b.state.Folders["INBOX"].Members["m00000"]
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = nil
	d, st, err := openDisk(dir, testEmail)
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	if st.History != "20" || st.Folders["INBOX"].Members["m00000"] != uid {
		t.Fatal("lightweight cursor/UID did not survive restart")
	}
}

func TestSkippedRetriesUseSeparateScheduleAndHistoryBypassesIt(t *testing.T) {
	f := largeFake(1)
	f.failID, f.errorGet = "m00000", &gmail.APIError{Status: 400, State: "INVALID_ARGUMENT"}
	var gets int
	h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		gets++
		return f.Message(ctx, id, format)
	}, changes: func(context.Context, string) ([]string, string, error) { return nil, "20", nil }}
	b := openAPI(t, t.TempDir(), h, Options{SkippedRetryInterval: time.Hour})
	defer b.Close()
	for n := 0; n < 4; n++ {
		if err := b.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if gets != 1 { // snapshot only: history polling never runs repair work
		t.Fatalf("skipped ID retried on fast polling schedule: gets=%d", gets)
	}
	if err := b.retrySkipped(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gets != 2 {
		t.Fatalf("independent pass did not retry skipped ID: gets=%d", gets)
	}
	f.errorGet = nil
	h.changes = func(context.Context, string) ([]string, string, error) { return []string{"m00000"}, "30", nil }
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gets != 3 || len(b.state.Skipped) != 0 || b.state.Messages["m00000"] == nil {
		t.Fatal("changed skipped message waited for slower retry schedule")
	}
}

func TestLabelRefreshAppliesRenameAndRemoval(t *testing.T) {
	f := largeFake(0)
	h := &hookedAPI{API: f}
	b := openAPI(t, t.TempDir(), h, Options{})
	defer b.Close()
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	validity := b.state.Folders["label1"].Validity
	h.labels = func(context.Context) ([]gmail.Label, error) {
		return []gmail.Label{{ID: "label1", Name: "Renamed", Type: "user"}}, nil
	}
	b.lastLabels = time.Time{}
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if folder := b.state.Folders["label1"]; folder.Name != "Labels/Renamed" || folder.Validity != validity {
		t.Fatal("label rename lost identity or was ignored")
	}
	h.labels = func(context.Context) ([]gmail.Label, error) { return nil, nil }
	b.lastLabels = time.Time{}
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.state.Folders["label1"].Active {
		t.Fatal("removed label remained active")
	}
}

func TestRunSyncStopInterruptsLongPollWait(t *testing.T) {
	b := openAPI(t, t.TempDir(), largeFake(0), Options{})
	defer b.Close()
	done := make(chan struct{})
	go func() { b.RunSync(context.Background(), time.Hour); close(done) }()
	b.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bridge stop left sync waiting for polling interval")
	}
}

func TestSystemicFailureKeepsCheckpointAndRetries(t *testing.T) {
	for _, status := range []int{400, 401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := largeFake(4)
			f.failID = "m00002"
			f.errorGet = &gmail.APIError{Status: status}
			h := &hookedAPI{API: f}
			b := openAPI(t, t.TempDir(), h, Options{Workers: 1, BatchSize: 1})
			defer b.Close()
			if err := b.Sync(context.Background()); err == nil {
				t.Fatal("systemic error ignored")
			}
			if b.state.Pending == nil || b.state.History != "" || len(b.state.Pending.Done) != 2 || len(b.state.Messages) != 2 {
				t.Fatal("partial work/checkpoint lost or promoted prematurely")
			}
			var fetched []string
			f.errorGet = nil
			h.get = func(ctx context.Context, id, format string) (*gmail.Message, error) {
				fetched = append(fetched, id)
				return f.Message(ctx, id, format)
			}
			if err := b.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if strings.Join(fetched, ",") != "m00002,m00003" || b.state.Pending != nil || b.state.History != "10" {
				t.Fatalf("resume refetched completed messages: %v", fetched)
			}
		})
	}
}

func TestCheckpointCrashHelper(t *testing.T) {
	dir := os.Getenv("MAILME_CRASH_DIR")
	if dir == "" {
		return
	}
	f := largeFake(700)
	h := &hookedAPI{API: f}
	h.get = func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if id == "m00260" {
			os.WriteFile(filepath.Join(dir, "ready"), []byte("checkpoint committed"), 0600)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return f.Message(ctx, id, format)
	}
	b, err := Open(dir, testEmail, testPassword, h, Options{Workers: 1})
	if err != nil {
		os.Exit(2)
	}
	err = b.Sync(context.Background())
	b.Close()
	if err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestCheckpointSurvivesSIGKILLAndCatchesInterveningChanges(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestCheckpointCrashHelper$")
	cmd.Env = append(os.Environ(), "MAILME_CRASH_DIR="+dir, "GORACE=atexit_sleep_ms=0")
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err = os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not checkpoint")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("helper unexpectedly completed")
	}
	f := largeFake(700)
	f.history = "20"
	m := f.messages["m00000"]
	m.LabelIDs = []string{"STARRED"}
	f.messages[m.ID] = m
	var mu sync.Mutex
	counts := make(map[string]int)
	h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		mu.Lock()
		counts[id]++
		mu.Unlock()
		return f.Message(ctx, id, format)
	}}
	b := openAPI(t, dir, h, Options{})
	defer b.Close()
	if b.state.Pending == nil || b.state.Pending.History != "10" || b.state.History != "" || len(b.state.Pending.Done) < 250 {
		t.Fatal("durable pending cursor/batch missing after SIGKILL")
	}
	completed := b.state.Pending.Done
	uid, validity := b.state.Folders["INBOX"].Members["m00000"], b.state.Folders["INBOX"].Validity
	if err = b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id := range completed {
		if counts[id] != 0 {
			t.Fatalf("refetched checkpointed ID %s", id)
		}
	}
	if b.state.History != "10" || b.state.Pending != nil || len(b.state.Messages) != 700 {
		t.Fatal("pending cursor not promoted intact")
	}
	if b.state.Folders["INBOX"].Members["m00000"] != uid || b.state.Folders["INBOX"].Validity != validity {
		t.Fatal("partial snapshot changed UID identity")
	}
	if err = b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.state.History != "20" || !contains(b.state.Messages["m00000"].Message.LabelIDs, "STARRED") || contains(b.state.Messages["m00000"].Message.LabelIDs, "INBOX") {
		t.Fatal("lost a Gmail change during interrupted backfill")
	}
}

func TestExpiredPendingCursorRestartsSnapshot(t *testing.T) {
	f := largeFake(3)
	h := &hookedAPI{API: f}
	b := openAPI(t, t.TempDir(), h, Options{Workers: 1})
	defer b.Close()
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	ids, _ := h.IDs(context.Background())
	if err := b.update(func(st *state) {
		st.Pending = &snapshot{History: "expired", Listed: true, IDs: ids, Done: map[string]bool{"m00000": true}, Touched: map[string]bool{}}
	}); err != nil {
		t.Fatal(err)
	}
	f.historyErr = &gmail.APIError{Status: 404}
	f.history = "30"
	var calls int
	h.get = func(ctx context.Context, id, format string) (*gmail.Message, error) {
		calls++
		return f.Message(ctx, id, format)
	}
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || b.state.Pending != nil || b.state.History != "30" {
		t.Fatal("expired pending cursor reused")
	}
}

func TestServePartialSnapshotAndStableLateUIDs(t *testing.T) {
	f := largeFake(3)
	started, release := make(chan struct{}), make(chan struct{})
	h := &hookedAPI{API: f, inbox: []string{"m00002"}}
	h.get = func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if id == "m00000" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return f.Message(ctx, id, format)
	}
	b := openAPI(t, t.TempDir(), h, Options{Workers: 1, BatchSize: 1})
	defer b.Close()
	ia, _, cfg := startServers(t, b)
	w := dialWire(t, ia, cfg)
	w.ok("LOGIN me@example.com " + testPassword)
	w.ok("SELECT INBOX") // works before any API sync, even with an empty cache
	done := make(chan error, 1)
	go func() { done <- b.Sync(context.Background()) }()
	<-started
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	w.conn.SetDeadline(time.Now().Add(time.Second))
	w.ok("NOOP")
	w.ok("SELECT INBOX")
	out := w.ok("FETCH 1 (UID)")
	if !strings.Contains(out, "UID 1") {
		t.Fatal(out)
	}
	b.mu.Lock()
	firstUID, validity := b.state.Folders["INBOX"].Members["m00002"], b.state.Folders["INBOX"].Validity
	b.mu.Unlock()
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if b.state.Folders["INBOX"].Members["m00002"] != firstUID || b.state.Folders["INBOX"].Members["m00000"] <= firstUID || b.state.Folders["INBOX"].Validity != validity {
		t.Fatal("late older message renumbered UIDs")
	}
}

func TestSyncDoesNotOverwriteConcurrentStore(t *testing.T) {
	f := newFake()
	b := openTest(t, f)
	started, release := make(chan struct{}), make(chan struct{})
	h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		m, err := f.Message(ctx, id, format)
		close(started)
		select {
		case <-release:
			return m, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	b.api = h
	done := make(chan error, 1)
	go func() { done <- b.Sync(context.Background()) }()
	<-started
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	s := b.NewSession()
	defer s.Close()
	if _, err := s.Select("INBOX", &imap.SelectOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store(nil, imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged, imap.FlagAnswered}, Silent: true}, nil); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r := b.state.Messages["a"]
	if !contains(r.Message.LabelIDs, "STARRED") || !r.Answered {
		t.Fatal("stale metadata overwrote local mutation")
	}
	if b.state.Skipped["a"] == "" {
		t.Fatal("overlapping result not scheduled for reconciliation")
	}
}

func TestRawSingleFlightAndForegroundResponsiveness(t *testing.T) {
	f := newFake()
	b := openTest(t, f)
	started, release := make(chan struct{}), make(chan struct{})
	var count atomic.Int32
	b.api = &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if format == "raw" {
			if count.Add(1) == 1 {
				close(started)
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return f.Message(ctx, id, format)
	}}
	ia, _, cfg := startServers(t, b)
	reader := dialWire(t, ia, cfg)
	other := dialWire(t, ia, cfg)
	for _, w := range []*wire{reader, other} {
		w.ok("LOGIN me@example.com " + testPassword)
		w.ok("SELECT INBOX")
	}
	done := make(chan string, 1)
	go func() { done <- reader.command("FETCH 1 (BODY.PEEK[])") }()
	<-started
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	other.conn.SetDeadline(time.Now().Add(time.Second))
	other.ok("NOOP")
	other.ok("SELECT INBOX")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := b.raw(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled single-flight waiter: %v", err)
	}
	unblock()
	if out := <-done; !strings.Contains(out, " OK ") {
		t.Fatal(out)
	}
	if count.Load() != 1 {
		t.Fatalf("duplicate raw fetches=%d", count.Load())
	}
}

func TestBackgroundSyncRetriesWithoutRestart(t *testing.T) {
	f := largeFake(1)
	var calls atomic.Int32
	h := &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if calls.Add(1) == 1 {
			return nil, &gmail.APIError{Status: 401}
		}
		return f.Message(ctx, id, format)
	}}
	b := openAPI(t, t.TempDir(), h, Options{})
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.RunSync(ctx, time.Hour); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		complete := b.state.History != ""
		b.mu.Unlock()
		if complete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background sync failed to recover")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProjectedHeadersAndArbitraryHeaderSearch(t *testing.T) {
	f := newFake()
	m := f.messages["a"]
	m.Payload = &gmail.Payload{Headers: []gmail.Header{{Name: "Subject", Value: "=?utf-8?Q?Hello_caf=C3=A9?="}, {Name: "From", Value: "sender@example.com"}}}
	m.Raw = base64.RawURLEncoding.EncodeToString([]byte("X-Project: hidden-custom-header\r\n" + fixture))
	f.messages["a"] = m
	b := openTest(t, f)
	var rawCalls atomic.Int32
	b.api = &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
		if format == "raw" {
			rawCalls.Add(1)
		}
		return f.Message(ctx, id, format)
	}}
	ia, _, cfg := startServers(t, b)
	w := dialWire(t, ia, cfg)
	w.ok("LOGIN me@example.com " + testPassword)
	w.ok("SELECT INBOX")
	w.ok("FETCH 1 (ENVELOPE)")
	if rawCalls.Load() != 0 {
		t.Fatal("ENVELOPE downloaded MIME despite cached headers")
	}
	if out := w.ok("SEARCH SUBJECT Hello"); !strings.Contains(out, "* SEARCH 1") {
		t.Fatal(out)
	}
	if rawCalls.Load() != 0 {
		t.Fatal("projected header search downloaded MIME")
	}
	if out := w.ok("SEARCH HEADER X-Project hidden-custom-header"); !strings.Contains(out, "* SEARCH 1") {
		t.Fatal(out)
	}
	if rawCalls.Load() != 1 {
		t.Fatal("arbitrary header search did not use full MIME")
	}
	if out := w.ok("SEARCH TEXT hidden-custom-header"); !strings.Contains(out, "* SEARCH 1") {
		t.Fatal(out)
	}
}

type slowMutation struct {
	API
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *slowMutation) Modify(ctx context.Context, id string, add, remove []string) (*gmail.Message, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
	}
	return s.API.Modify(ctx, id, add, remove)
}
func TestMutationAndSearchDoNotHoldStateLock(t *testing.T) {
	for _, command := range []string{"UID STORE 1 +FLAGS.SILENT (\\Flagged)", `COPY 1 "Labels/Projects/Test"`, "EXPUNGE", "SEARCH TEXT Hello"} {
		t.Run(command, func(t *testing.T) {
			f := newFake()
			b := openTest(t, f)
			entered, release := make(chan struct{}), make(chan struct{})
			ia, _, cfg := startServers(t, b)
			first, second := dialWire(t, ia, cfg), dialWire(t, ia, cfg)
			for _, w := range []*wire{first, second} {
				w.ok("LOGIN me@example.com " + testPassword)
				w.ok("SELECT INBOX")
			}
			if command == "EXPUNGE" {
				first.ok("UID STORE 1 +FLAGS.SILENT (\\Deleted)")
			}
			if strings.HasPrefix(command, "SEARCH") {
				b.api = &hookedAPI{API: f, get: func(ctx context.Context, id, format string) (*gmail.Message, error) {
					close(entered)
					select {
					case <-release:
						return f.Message(ctx, id, format)
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}}
			} else {
				b.api = &slowMutation{API: f, entered: entered, release: release}
			}
			done := make(chan string, 1)
			go func() { done <- first.command(command) }()
			<-entered
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			second.conn.SetDeadline(time.Now().Add(time.Second))
			second.ok("NOOP")
			second.ok("SELECT INBOX")
			unblock()
			if out := <-done; !strings.Contains(out, " OK ") {
				t.Fatal(out)
			}
		})
	}
}

func TestLargeMailboxBackfill(t *testing.T) {
	if os.Getenv("MAILME_BENCHMARK") != "1" {
		t.Skip("opt-in 16,650-message disk/index scale check")
	}
	f := largeFake(16650)
	for id, m := range f.messages {
		m.Payload = &gmail.Payload{Headers: []gmail.Header{{Name: "From", Value: "sender@example.com"}, {Name: "To", Value: testEmail}, {Name: "Subject", Value: "Synthetic scale test"}, {Name: "Date", Value: "Sat, 24 Oct 2026 10:00:00 -0700"}, {Name: "Message-ID", Value: "<" + id + "@example.com>"}}}
		f.messages[id] = m
	}
	b := openAPI(t, t.TempDir(), &hookedAPI{API: f}, Options{})
	defer b.Close()
	start := time.Now()
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.state.Messages) != 16650 || len(b.state.Folders["INBOX"].Members) != 16650 || b.state.Pending != nil {
		t.Fatal("large snapshot incomplete")
	}
	t.Logf("SYNTHETIC zero-API-latency scale check: 16,650 messages with metadata headers, elapsed=%s (includes batch persistence/reconciliation)", time.Since(start).Round(time.Millisecond))
}
