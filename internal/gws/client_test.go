package gws

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailme/internal/gmail"
)

type invocation struct {
	Args       []string
	Upload     string
	Raw        []byte
	FileMode   uint32
	ParentMode uint32
	Started    int64
}

// Run the test binary as a fake gws. Tests exercise actual subprocess arguments,
// exit status, bounded pipes, cancellation, and filesystem behavior, offline.
func TestGWSHelperProcess(t *testing.T) {
	if os.Getenv("MAILME_TEST_GWS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	flag := func(name string) string {
		for i, arg := range args {
			if arg == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	var params map[string]any
	if json.Unmarshal([]byte(flag("--params")), &params) != nil {
		os.Exit(3)
	}
	path := os.Getenv("MAILME_TEST_LOG")
	previous, _ := os.ReadFile(path)
	entry := invocation{Args: args, Upload: flag("--upload"), Started: time.Now().UnixNano()}
	if entry.Upload != "" {
		entry.Raw, _ = os.ReadFile(entry.Upload)
		if stat, err := os.Stat(entry.Upload); err == nil {
			entry.FileMode = uint32(stat.Mode().Perm())
		}
		if stat, err := os.Stat(filepath.Dir(entry.Upload)); err == nil {
			entry.ParentMode = uint32(stat.Mode().Perm())
		}
	}
	data, _ := json.Marshal(entry)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(5)
	}
	f.Write(append(data, '\n'))
	f.Close()
	emit := func(s string, code int) {
		fmt.Fprint(os.Stdout, s)
		os.Exit(code)
	}
	scenario := os.Getenv("MAILME_TEST_SCENARIO")
	if scenario == "concurrent" || scenario == "auth-recover" {
		ready := path + ".ready"
		_, statErr := os.Stat(ready)
		if scenario == "auth-recover" && args[2] == "getProfile" {
			os.WriteFile(ready, []byte("ready"), 0600)
		}
		delay := 250
		if v, err := strconv.Atoi(os.Getenv("MAILME_TEST_DELAY_MS")); err == nil {
			delay = v
		}
		time.Sleep(time.Duration(delay) * time.Millisecond)
		if scenario == "auth-recover" && args[2] != "getProfile" && os.IsNotExist(statErr) {
			emit(`{"error":{"code":401,"reason":"authError"}}`, 1)
		}
	}
	switch scenario {
	case "rate-limit":
		if len(previous) == 0 {
			emit(`{"error":{"code":403,"status":"RESOURCE_EXHAUSTED","errors":[{"reason":"userRateLimitExceeded"}],"message":"PRIVATE TOKEN"}}`, 1)
		}
	case "slow":
		time.Sleep(10 * time.Minute)
	case "oversize":
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for i := 0; i < 66; i++ {
			os.Stdout.Write(chunk)
		}
		os.Exit(0)
	case "503", "404":
		fmt.Fprintln(os.Stderr, "PRIVATE TOKEN AND MESSAGE")
		emit(`{"error":{"code":`+scenario+`,"message":"PRIVATE TOKEN AND MESSAGE"}}`, 1)
	case "retry":
		if len(previous) == 0 {
			emit(`{"error":{"code":503,"message":"busy"}}`, 1)
		}
	case "error-zero-exit":
		emit(`{"error":{"code":401,"message":"PRIVATE TOKEN"}}`, 0)
	case "error-nonjson":
		emit("PRIVATE TOKEN", 2)
	case "success-nonzero":
		emit(`{"id":"sent"}`, 5)
	case "malformed":
		emit("PRIVATE MESSAGE", 0)
	case "empty":
		emit(`{}`, 0)
	case "null":
		emit(`null`, 0)
	case "mismatch":
		emit(`{"emailAddress":"other@example.com","historyId":"123"}`, 0)
	case "multiple":
		emit("{}\n{}", 0)
	}
	if len(args) < 4 || args[0] != "gmail" || args[1] != "users" || params["userId"] != "me@example.com" || flag("--format") != "json" {
		emit(`{"error":{"code":400,"message":"bad command or account"}}`, 3)
	}
	if args[2] == "getProfile" {
		emit(`{"emailAddress":"me@example.com","historyId":"123"}`, 0)
	}
	switch args[2] + "/" + args[3] {
	case "labels/list":
		emit(`{"labels":[{"id":"INBOX","name":"INBOX","type":"system"}]}`, 0)
	case "messages/list":
		if params["includeSpamTrash"] != true || params["maxResults"] != float64(500) {
			os.Exit(3)
		}
		page := 0
		if token, ok := params["pageToken"].(string); ok {
			page, _ = strconv.Atoi(strings.TrimPrefix(token, "page+"))
		}
		r := map[string]any{"messages": []map[string]string{{"id": fmt.Sprint(page)}}}
		if page < 11 {
			r["nextPageToken"] = fmt.Sprintf("page+%d", page+1)
		}
		if scenario == "repeated-page" {
			r["nextPageToken"] = "page+1"
		}
		b, _ := json.Marshal(r)
		emit(string(b), 0)
	case "history/list":
		if params["startHistoryId"] != "123" || params["maxResults"] != float64(500) {
			os.Exit(3)
		}
		if params["pageToken"] == nil {
			emit(`{"history":[{"messages":[{"id":"a"}]}],"nextPageToken":"page+2","historyId":"124"}`, 0)
		}
		if scenario == "partial-history" {
			emit(`{"error":{"code":403,"message":"denied"}}`, 1)
		}
		emit(`{"history":[{"messages":[{"id":"a"},{"id":"b"}]}],"historyId":"130"}`, 0)
	case "messages/get", "messages/modify", "messages/trash", "messages/untrash":
		if args[3] == "get" {
			if params["format"] == "metadata" {
				if params["fields"] != gmail.MetadataFields || len(params["metadataHeaders"].([]any)) < 11 {
					os.Exit(3)
				}
			} else if params["format"] != "raw" {
				os.Exit(3)
			}
		}
		if args[3] == "modify" {
			var body map[string]any
			json.Unmarshal([]byte(flag("--json")), &body)
			if body["addLabelIds"] == nil || body["removeLabelIds"] == nil {
				os.Exit(3)
			}
		}
		if (args[3] == "trash" || args[3] == "untrash") && flag("--json") != "" {
			os.Exit(3) // These Discovery methods have no JSON request body.
		}
		b, _ := json.Marshal(map[string]any{"id": params["id"], "labelIds": []string{"INBOX"}, "internalDate": "1000", "raw": base64.RawURLEncoding.EncodeToString([]byte("MIME\r\n"))})
		emit(string(b), 0)
	case "messages/send":
		if entry.Upload == "" || flag("--upload-content-type") != "message/rfc822" || flag("--json") != "" {
			os.Exit(3)
		}
		emit(`{"id":"sent"}`, 0)
	}
	os.Exit(3)
}

func fakeCLI(t *testing.T, scenario string) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.jsonl")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fake gws")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "' -test.run='^TestGWSHelperProcess$' -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILME_TEST_GWS", "1")
	t.Setenv("MAILME_TEST_LOG", logPath)
	t.Setenv("MAILME_TEST_SCENARIO", scenario)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	c, err := New(path, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Most command contract tests start with an already-warmed credential cache.
	// Dedicated concurrency tests exercise startup and 401 recovery probes.
	c.authReady = true
	return c, logPath
}

func calls(t *testing.T, path string) []invocation {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result []invocation
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var entry invocation
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		result = append(result, entry)
	}
	return result
}

func TestGWSMailboxOperations(t *testing.T) {
	c, _ := fakeCLI(t, "normal")
	ctx := context.Background()
	if p, err := c.Profile(ctx); err != nil || p.Email != "me@example.com" {
		t.Fatalf("profile: %v %v", p, err)
	}
	if labels, err := c.Labels(ctx); err != nil || len(labels) != 1 || labels[0].ID != "INBOX" {
		t.Fatalf("labels: %v %v", labels, err)
	}
	ids, err := c.IDs(ctx)
	if err != nil || len(ids) != 12 || ids[11] != "11" {
		t.Fatalf("pagination (must not stop at CLI's default page cap): %v %v", ids, err)
	}
	changed, cursor, err := c.Changes(ctx, "123")
	sort.Strings(changed)
	if err != nil || !reflect.DeepEqual(changed, []string{"a", "b"}) || cursor != "130" {
		t.Fatalf("history: %v %s %v", changed, cursor, err)
	}
	// IDs remain JSON values even when they contain shell/flag metacharacters.
	id := "opaque+id;$(false) --flag"
	if m, err := c.Message(ctx, id, "raw"); err != nil || m.ID != id || m.Raw == "" {
		t.Fatalf("get: %v %v", m, err)
	}
	if _, err := c.Modify(ctx, id, nil, []string{"UNREAD"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Trash(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Untrash(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestGWSSendUsesPrivateUploadAndNeverRetries(t *testing.T) {
	for _, scenario := range []string{"normal", "503", "empty", "success-nonzero"} {
		t.Run(scenario, func(t *testing.T) {
			c, logPath := fakeCLI(t, scenario)
			raw := append([]byte("From: me@example.com\r\nBcc: hidden@example.com\r\n\r\nPRIVATE MESSAGE\r\n"), bytes.Repeat([]byte("x\x00"), 1<<20)...)
			err := c.Send(context.Background(), raw)
			if (err != nil) != (scenario != "normal") {
				t.Fatalf("send: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("leaked CLI output: %v", err)
			}
			entries := calls(t, logPath)
			if len(entries) != 1 {
				t.Fatalf("mutation retried: %d invocations", len(entries))
			}
			entry := entries[0]
			if !bytes.Equal(entry.Raw, raw) || entry.FileMode != 0600 || entry.ParentMode != 0700 {
				t.Fatal("upload changed MIME or used unsafe permissions")
			}
			if strings.Contains(strings.Join(entry.Args, " "), "PRIVATE") || len(strings.Join(entry.Args, " ")) > 4096 {
				t.Fatal("MIME leaked into process arguments")
			}
			if _, err := os.Stat(filepath.Dir(entry.Upload)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("upload directory remains: %v", err)
			}
		})
	}
}

func TestGWSReadRetryAndErrorHandling(t *testing.T) {
	t.Run("read retry", func(t *testing.T) {
		c, logPath := fakeCLI(t, "retry")
		if _, err := c.Profile(context.Background()); err != nil || len(calls(t, logPath)) != 2 {
			t.Fatalf("retry: %v", err)
		}
	})
	t.Run("history status", func(t *testing.T) {
		c, logPath := fakeCLI(t, "404")
		ids, cursor, err := c.Changes(context.Background(), "123")
		var apiErr *gmail.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 404 || ids != nil || cursor != "" || len(calls(t, logPath)) != 1 {
			t.Fatalf("history status lost: %v %s %v", ids, cursor, err)
		}
	})
	for _, scenario := range []string{"error-zero-exit", "error-nonjson", "malformed", "empty", "null", "mismatch", "multiple", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			c, logPath := fakeCLI(t, scenario)
			_, err := c.Profile(context.Background())
			want := 1
			if scenario == "error-zero-exit" {
				want = 2
			}
			if err == nil || strings.Contains(err.Error(), "PRIVATE") || len(calls(t, logPath)) != want {
				t.Fatalf("invalid response accepted, retried, or leaked: %v", err)
			}
		})
	}
	t.Run("partial history", func(t *testing.T) {
		c, _ := fakeCLI(t, "partial-history")
		ids, cursor, err := c.Changes(context.Background(), "123")
		if err == nil || ids != nil || cursor != "" {
			t.Fatalf("partial history committed: %v %s %v", ids, cursor, err)
		}
	})
	t.Run("pagination cycle", func(t *testing.T) {
		c, logPath := fakeCLI(t, "repeated-page")
		if ids, err := c.IDs(context.Background()); err == nil || ids != nil || len(calls(t, logPath)) != 2 {
			t.Fatalf("page cycle: %v %v", ids, err)
		}
	})
}

func TestGWSCancellationCleansUploadAndQueuedCalls(t *testing.T) {
	c, logPath := fakeCLI(t, "slow")
	c.gate = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Send(ctx, []byte("MIME\r\n")) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := os.ReadFile(logPath); bytes.Contains(b, []byte{'\n'}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake CLI did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	queued, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if _, err := c.Profile(queued); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued cancellation: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("running cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled CLI kept running")
	}
	entries := calls(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("queued or cancelled call executed again: %d", len(entries))
	}
	if _, err := os.Stat(filepath.Dir(entries[0].Upload)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled upload remains: %v", err)
	}
}
