package gws

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"mailme/internal/gmail"
)

func parallelReads(t *testing.T, c *Client, n int) {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := c.Message(context.Background(), id, "metadata"); err != nil {
				t.Error(err)
			}
		}(fmt.Sprint(i))
	}
	wg.Wait()
}

func TestReadConcurrencyAndCredentialProbes(t *testing.T) {
	t.Run("bounded parallel reads and startup", func(t *testing.T) {
		c, path := fakeCLI(t, "concurrent")
		c.gate = make(chan struct{}, 4)
		c.authReady = false
		parallelReads(t, c, 12)
		entries := calls(t, path)
		if len(entries) != 13 || entries[0].Args[2] != "getProfile" {
			t.Fatalf("startup probe: %d calls", len(entries))
		}
		starts := make([]int64, 0, 12)
		for _, e := range entries[1:] {
			starts = append(starts, e.Started)
		}
		sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
		max := 0
		for i, start := range starts {
			n := 1
			for j := i - 1; j >= 0 && start-starts[j] < int64(250*time.Millisecond); j-- {
				n++
			}
			if n > max {
				max = n
			}
		}
		if max < 3 || max > 4 {
			t.Fatalf("parallel subprocesses=%d, want 3–4", max)
		}
		if starts[0]-entries[0].Started < int64(250*time.Millisecond) {
			t.Fatal("read raced initial credential probe")
		}
	})
	t.Run("one recovery probe for simultaneous 401s", func(t *testing.T) {
		c, path := fakeCLI(t, "auth-recover")
		c.gate = make(chan struct{}, 4)
		parallelReads(t, c, 4)
		probes, reads := 0, 0
		for _, e := range calls(t, path) {
			if e.Args[2] == "getProfile" {
				probes++
			} else {
				reads++
			}
		}
		if probes != 1 || reads != 8 {
			t.Fatalf("probes=%d reads=%d; want 1 shared probe and one retry/read", probes, reads)
		}
	})
	t.Run("cancel while waiting for credential probe", func(t *testing.T) {
		c, path := fakeCLI(t, "slow")
		c.authReady = false
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := c.Profile(ctx); done <- err }()
		defer func() { cancel(); <-done }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if data, _ := os.ReadFile(path); len(data) > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("probe did not start")
			}
			time.Sleep(10 * time.Millisecond)
		}
		waitCtx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer stop()
		start := time.Now()
		if _, err := c.Profile(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("cancellation stuck behind auth barrier")
		}
	})
}

func TestGWSRateLimitAndSerializedWrites(t *testing.T) {
	t.Run("403 read retries", func(t *testing.T) {
		c, path := fakeCLI(t, "rate-limit")
		if _, err := c.Profile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(calls(t, path)) != 2 {
			t.Fatal("403 rate limit was not retried")
		}
	})
	t.Run("403 write never retries", func(t *testing.T) {
		c, path := fakeCLI(t, "rate-limit")
		_, err := c.Modify(context.Background(), "a", []string{"STARRED"}, nil)
		var apiErr *gmail.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 403 || apiErr.Method != "messages.modify" || apiErr.ID != "a" || apiErr.State != "RESOURCE_EXHAUSTED" || !strings.Contains(err.Error(), "userRateLimitExceeded") || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("safe details lost: %v", err)
		}
		if len(calls(t, path)) != 1 {
			t.Fatal("write retried")
		}
	})
	t.Run("writes serialized", func(t *testing.T) {
		c, path := fakeCLI(t, "concurrent")
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := c.Trash(context.Background(), "a"); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		entries := calls(t, path)
		for i := 1; i < len(entries); i++ {
			if entries[i].Started-entries[i-1].Started < int64(250*time.Millisecond) {
				t.Fatal("overlapping writes")
			}
		}
	})
}

func TestNativeNPMBinaryResolution(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "@googleworkspace", "cli")
	os.MkdirAll(filepath.Join(root, "bin"), 0700)
	os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"@googleworkspace/cli"}`), 0600)
	os.WriteFile(filepath.Join(root, "run.js"), []byte("#!/usr/bin/env node\n"), 0700)
	native := filepath.Join(root, "bin", "gws")
	os.WriteFile(native, []byte{0xcf, 0xfa, 0xed, 0xfe}, 0700)
	shim := filepath.Join(dir, "gws")
	if err := os.Symlink(filepath.Join(root, "run.js"), shim); err != nil {
		t.Fatal(err)
	}
	c, err := New(shim, "me@example.com")
	resolved, _ := filepath.EvalSymlinks(native)
	if err != nil || c.Path() != resolved {
		t.Fatalf("resolved path: %v %v", c, err)
	}
	os.Remove(native)
	if _, err := New(shim, "me@example.com"); err == nil || !strings.Contains(err.Error(), "Node shim") {
		t.Fatalf("missing native binary not explained: %v", err)
	}
}

// Explicit opt-in benchmark with the reported 370ms CLI request latency. It
// invokes only this test executable, never the installed gws or a real account.
func TestSimulatedThroughput(t *testing.T) {
	if os.Getenv("MAILME_BENCHMARK") != "1" {
		t.Skip("opt-in simulated latency benchmark")
	}
	for _, concurrency := range []int{1, 6} {
		t.Run(fmt.Sprint(concurrency), func(t *testing.T) {
			c, _ := fakeCLI(t, "concurrent")
			c.gate = make(chan struct{}, concurrency)
			t.Setenv("MAILME_TEST_DELAY_MS", "370")
			start := time.Now()
			parallelReads(t, c, 60)
			elapsed := time.Since(start)
			t.Logf("SIMULATED: concurrency=%d fetched=60 elapsed=%s rate=%.2f messages/s projected-16650=%s", concurrency, elapsed.Round(time.Millisecond), 60/elapsed.Seconds(), (elapsed * 16650 / 60).Round(time.Second))
		})
	}
}
