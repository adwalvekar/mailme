package logging

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	o := Default()
	o.File = filepath.Join(t.TempDir(), "logs", "bridge.log")
	return o
}

func TestPersistentStructuredLogsAndLevel(t *testing.T) {
	o := testOptions(t)
	o.Format, o.Level, o.Stderr = "json", "warn", false
	var console bytes.Buffer
	s, err := Open(o, &console)
	if err != nil {
		t.Fatal(err)
	}
	s.Logger.Info("filtered progress")
	s.Logger.Warn("sync paused", "id", "abc123", "reason", "rateLimitExceeded")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(o.File)
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if entry["level"] != "WARN" || entry["msg"] != "sync paused" || entry["id"] != "abc123" || console.Len() != 0 || strings.Contains(string(data), "filtered") {
		t.Fatalf("structured logging/filtering: %s", data)
	}
	info, _ := os.Stat(o.File)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("log permissions: %v", info.Mode())
	}
	dirInfo, _ := os.Stat(filepath.Dir(o.File))
	if dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("new directory permissions: %v", dirInfo.Mode())
	}
}

func TestRotationCompressionAndRetention(t *testing.T) {
	o := testOptions(t)
	o.MaxSizeMB, o.MaxBackups, o.MaxAgeDays = 1, 2, 1
	if err := os.MkdirAll(filepath.Dir(o.File), 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(filepath.Dir(o.File), "bridge-2000-01-01T00-00-00.000.log")
	if err := os.WriteFile(old, []byte("expired"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(o, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunk := bytes.Repeat([]byte("synthetic log\n"), 50000)
	for n := 0; n < 8; n++ {
		if _, err := s.writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct upstream timestamp filenames
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		files, err := filepath.Glob(filepath.Join(filepath.Dir(o.File), "bridge-*.log*"))
		if err != nil {
			t.Fatal(err)
		}
		complete := len(files) == o.MaxBackups
		for _, file := range files {
			complete = complete && strings.HasSuffix(file, ".gz")
		}
		if complete {
			for _, file := range files {
				f, err := os.Open(file)
				if err != nil {
					t.Fatal(err)
				}
				z, err := gzip.NewReader(f)
				if err != nil {
					f.Close()
					t.Fatal(err)
				}
				data, err := io.ReadAll(z)
				z.Close()
				f.Close()
				if err != nil || len(data) > 1<<20 || !bytes.Contains(data, []byte("synthetic log")) {
					t.Fatalf("invalid rotated log: %v", err)
				}
				info, _ := os.Stat(file)
				if info.Mode().Perm() != 0600 {
					t.Fatal("compressed log lost private permissions")
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retention/compression did not finish: %v", files)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired backup retained")
	}
	info, _ := os.Stat(o.File)
	if info.Size() > 1<<20 {
		t.Fatal("active log exceeded size limit")
	}
}

func TestExistingOversizedLogAndExclusiveWriter(t *testing.T) {
	o := testOptions(t)
	o.MaxSizeMB, o.Compress = 1, false
	if err := os.MkdirAll(filepath.Dir(o.File), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.File, bytes.Repeat([]byte("x"), 2<<20), 0644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(o, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := Open(o, io.Discard); err == nil {
		duplicate.Close()
		t.Fatal("two processes may rotate the same file")
	}
	s.Logger.Info("after upgrade")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(o.File)
	if info.Size() > 1<<20 || info.Mode().Perm() != 0600 {
		t.Fatal("oversized legacy log was not rotated privately")
	}
	reopened, err := Open(o, io.Discard)
	if err != nil {
		t.Fatal("writer lock was not released:", err)
	}
	reopened.Close()
}

func TestConcurrentLogging(t *testing.T) {
	o := testOptions(t)
	o.Format, o.Stderr = "json", false
	s, err := Open(o, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				s.Logger.Info("event", "worker", n, "index", i)
			}
		}(n)
	}
	wg.Wait()
	s.Close()
	data, _ := os.ReadFile(o.File)
	entries := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(entries) != 240 {
		t.Fatalf("lost log records: %d", len(entries))
	}
	for _, entry := range entries {
		if !json.Valid(entry) {
			t.Fatalf("interleaved log entry: %q", entry)
		}
	}
}

func TestInvalidLoggingOptions(t *testing.T) {
	for _, change := range []func(*Options){
		func(o *Options) { o.MaxSizeMB = 0 }, func(o *Options) { o.MaxBackups = 0 },
		func(o *Options) { o.MaxAgeDays = -1 }, func(o *Options) { o.Level = "trace" }, func(o *Options) { o.Format = "xml" },
	} {
		o := testOptions(t)
		change(&o)
		if o.Validate() == nil {
			t.Fatalf("invalid log settings accepted: %+v", o)
		}
	}
}

func TestTailLastLines(t *testing.T) {
	for _, text := range []string{"one\ntwo\nthree\n", "one\ntwo\nthree"} {
		file := filepath.Join(t.TempDir(), "bridge.log")
		if err := os.WriteFile(file, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := Tail(context.Background(), file, &out, 2, false); err != nil {
			t.Fatal(err)
		}
		if out.String() != strings.TrimPrefix(text, "one\n") {
			t.Fatalf("wrong tail: %q", out.String())
		}
	}
}

type observedOutput struct{ lines chan string }

func (o observedOutput) Write(p []byte) (int, error) { o.lines <- string(p); return len(p), nil }

func TestTailFollowsRotationAndCancellation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bridge.log")
	if err := os.WriteFile(file, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := observedOutput{make(chan string, 20)}
	done := make(chan error, 1)
	go func() { done <- Tail(ctx, file, output, 10, true) }()
	read := func(want string) {
		t.Helper()
		select {
		case got := <-output.lines:
			if got != want {
				t.Fatalf("tail got %q want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("tail missed %q", want)
		}
	}
	read("before\n")
	if err := os.Rename(file, file+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	read("after\n")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("tail ignored cancellation")
	}
}
