package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	api, err := newAPI(context.Background(), c)
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

func TestGWSLoginRejectsWrongAccountAndInvalidBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gws")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '{\"emailAddress\":\"other@example.com\",\"historyId\":\"123\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if c, err := connectGWS(context.Background(), path, "me@example.com"); err == nil || c.Email != "" {
		t.Fatalf("wrong account accepted: %+v %v", c, err)
	}
	if _, err := newAPI(context.Background(), auth.Config{Backend: "typo"}); err == nil {
		t.Fatal("unknown saved backend accepted")
	}
	if _, err := connectGWS(context.Background(), filepath.Join(t.TempDir(), "missing-gws"), "me@example.com"); err == nil {
		t.Fatal("missing executable accepted")
	}
}
