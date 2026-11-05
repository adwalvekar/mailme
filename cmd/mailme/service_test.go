package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
	"mailme/internal/auth"
	"mailme/internal/launchagent"
)

func TestForegroundStartupFailureIsPersisted(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit foreground", true: "older launchd service"}[managed], func(t *testing.T) {
			oldArgs, oldFactory, oldWriter := os.Args, newServiceManager, log.Writer()
			t.Cleanup(func() { os.Args = oldArgs; newServiceManager = oldFactory })
			newServiceManager = func(string) launchagent.Manager {
				t.Fatal("foreground service attempted to install itself")
				return launchagent.Manager{}
			}
			file := filepath.Join(t.TempDir(), "logs", "bridge.log")
			os.Args = []string{"mailme", "serve", "--data-dir", t.TempDir(), "--log-file", file, "--log-stderr=false", "--log-format", "json"}
			if managed {
				t.Setenv("XPC_SERVICE_NAME", launchagent.Label)
			} else {
				os.Args = append(os.Args, "--foreground")
			}
			if err := run(); err == nil {
				t.Fatal("missing login was not reported")
			}
			data, err := os.ReadFile(file)
			if err != nil || !strings.Contains(string(data), `"level":"ERROR"`) || !strings.Contains(string(data), "run login first") {
				t.Fatalf("startup failure lost: %s %v", data, err)
			}
			if log.Writer() != oldWriter {
				t.Fatal("logging was not restored on shutdown")
			}
		})
	}
}

func TestServeStartsBackgroundByDefaultWithoutCredentialsOrMailboxIO(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS service entrypoint")
	}
	dir, home := t.TempDir(), t.TempDir()
	if err := auth.Save(dir, auth.Config{Email: "me@example.com", Backend: "gws"}); err != nil {
		t.Fatal(err)
	}
	keyring.MockInitWithError(errors.New("parent must not read Keychain"))
	t.Cleanup(keyring.MockInit)
	oldArgs, oldFactory := os.Args, newServiceManager
	t.Cleanup(func() { os.Args = oldArgs; newServiceManager = oldFactory })
	var commands []string
	newServiceManager = func(string) launchagent.Manager {
		return launchagent.Manager{Home: home, UID: 501, Run: func(ctx context.Context, args ...string) ([]byte, error) {
			commands = append(commands, args[0])
			// Pretend a loaded job exists; tests never call the real launchctl.
			return []byte("state = running\npid = 123"), nil
		}}
	}
	file := filepath.Join(home, "logs", "bridge.log")
	os.Args = []string{"mailme", "serve", "--data-dir", dir, "--log-file", file, "--log-max-size", "2", "--log-compress=false"}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(commands, ",") != "print,bootout,bootstrap" {
		t.Fatalf("default did not start service: %v", commands)
	}
	data, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", launchagent.Label+".plist"))
	if err != nil || !strings.Contains(string(data), "<string>--foreground</string>") || !strings.Contains(string(data), "<string>--log-compress=false</string>") {
		t.Fatalf("service configuration lost: %s %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mail.db")); !os.IsNotExist(err) {
		t.Fatal("parent opened the mailbox")
	}
}

// Runs only as a child of the hermetic headless-service lifecycle test.
func TestForegroundServiceHelper(t *testing.T) {
	dir := os.Getenv("MAILME_TEST_SERVICE_DIR")
	if dir == "" {
		return
	}
	keyring.MockInit()
	if _, err := auth.EnsurePassword("me@example.com"); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, "fake-gws")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf '%s' '{\"emailAddress\":\"me@example.com\",\"historyId\":\"10\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := auth.Save(dir, auth.Config{Email: "me@example.com", Backend: "gws", GWSPath: cli}); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"mailme", "serve", "--foreground", "--data-dir", dir, "--log-file", filepath.Join(dir, "bridge.log"), "--log-format", "json", "--log-stderr=false", "--imap", "127.0.0.1:0", "--smtp", "127.0.0.1:0"}
	if err := run(); err != nil {
		t.Fatal(err)
	}
}

func TestHeadlessServicePersistsStartupAndShutdown(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestForegroundServiceHelper$")
	cmd.Env = append(os.Environ(), "MAILME_TEST_SERVICE_DIR="+dir, "GORACE=atexit_sleep_ms=0")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	ready := false
	for !ready {
		select {
		case err := <-done:
			t.Fatalf("headless child exited before ready: %v", err)
		case <-deadline.C:
			t.Fatal("headless child never persisted readiness")
		case <-ticker.C:
			data, _ := os.ReadFile(filepath.Join(dir, "bridge.log"))
			ready = strings.Contains(string(data), "Bridge ready")
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("headless service failed to stop:", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("headless service did not stop on SIGTERM")
	}
	data, err := os.ReadFile(filepath.Join(dir, "bridge.log"))
	if err != nil || !strings.Contains(string(data), "Starting bridge") || !strings.Contains(string(data), "Bridge stopped") {
		t.Fatalf("headless lifecycle logs missing: %s %v", data, err)
	}
}
