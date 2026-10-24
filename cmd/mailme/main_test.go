package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
	"mailme/internal/auth"
)

func TestGWSLoginAndSavedBackend(t *testing.T) {
	keyring.MockInit()
	path := filepath.Join(t.TempDir(), "gws")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '{\"emailAddress\":\"me@example.com\",\"historyId\":\"123\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs; keyring.MockInit() })
	t.Setenv("PATH", filepath.Dir(path))
	os.Args = []string{"mailme", "login", "--backend", "gws", "--email", "ME@example.com", "--data-dir", dir}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	c, err := auth.Load(dir)
	if err != nil || c.BackendName() != "gws" || c.GWSPath != path || c.Email != "me@example.com" || c.ClientID != "" || c.ClientSecret != "" {
		t.Fatalf("saved gws config: %+v %v", c, err)
	}
	if password, err := auth.Password(c.Email); err != nil || len(password) < 24 {
		t.Fatalf("local bridge password missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "localhost.crt")); err != nil {
		t.Fatal("local TLS identity missing:", err)
	}
	// Startup must use the saved executable without consulting Google OAuth
	// tokens in Keychain or requiring the interactive shell's PATH.
	t.Setenv("PATH", t.TempDir())
	keyring.MockInitWithError(errors.New("OAuth Keychain access must not happen"))
	api, err := newAPI(context.Background(), c, 6, "current")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Profile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Cannot repoint a populated cache at another account during login.
	os.Args = []string{"mailme", "login", "--backend", "gws", "--gws-path", path, "--email", "other@example.com", "--data-dir", dir}
	if err := run(); err == nil {
		t.Fatal("changed account in an existing directory")
	}
}

func TestSettingsCopiesWithoutPrintingPassword(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	dir := t.TempDir()
	if err := auth.Save(dir, auth.Config{Email: "me@example.com", Backend: "gws"}); err != nil {
		t.Fatal(err)
	}
	password, err := auth.EnsurePassword("me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// Test-only clipboard sink; never invoke the real pbcopy.
	if err := os.WriteFile(filepath.Join(bin, "pbcopy"), []byte("#!/bin/sh\nexec /bin/cat > \"$MAILME_TEST_CLIPBOARD\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	clipboard := filepath.Join(t.TempDir(), "clipboard")
	t.Setenv("PATH", bin)
	t.Setenv("MAILME_TEST_CLIPBOARD", clipboard)
	oldArgs, oldStdout := os.Args, os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"mailme", "settings", "--data-dir", dir, "--copy-password"}
	os.Stdout = w
	t.Cleanup(func() { os.Args = oldArgs; os.Stdout = oldStdout; r.Close(); w.Close() })
	err = run()
	w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	output, _ := io.ReadAll(r)
	if strings.Contains(string(output), password) || !strings.Contains(string(output), "copied to clipboard") {
		t.Fatal("password was printed or clipboard status missing")
	}
	got, _ := os.ReadFile(clipboard)
	if string(got) != password {
		t.Fatal("clipboard sink did not receive password")
	}
}

func TestGWSLoginRejectsWrongAccountAndInvalidBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gws")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '{\"emailAddress\":\"other@example.com\",\"historyId\":\"123\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if c, err := connectGWS(context.Background(), path, "me@example.com"); err == nil || c.Email != "" {
		t.Fatalf("wrong account accepted: %+v %v", c, err)
	}
	if _, err := newAPI(context.Background(), auth.Config{Backend: "typo"}, 6, "current"); err == nil {
		t.Fatal("unknown saved backend accepted")
	}
	if _, err := newAPI(context.Background(), auth.Config{Backend: "gws"}, 6, "typo"); err == nil {
		t.Fatal("unknown quota profile accepted")
	}
	if _, err := connectGWS(context.Background(), filepath.Join(t.TempDir(), "missing-gws"), "me@example.com"); err == nil {
		t.Fatal("missing executable accepted")
	}
}
