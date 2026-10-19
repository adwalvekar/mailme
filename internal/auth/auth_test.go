package auth

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOAuthStateAndPKCE(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := Authorize(ctx, Config{Email: "me@example.com", ClientID: "test"}, func(link string) {
		u, e := url.Parse(link)
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("scope") != Scope {
			t.Error("missing PKCE/scope")
		}
		redirect := q.Get("redirect_uri")
		res, e := http.Get(redirect + "?state=wrong&code=untrusted")
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Error("wrong state accepted")
		}
		res, e = http.Get(redirect + "?state=" + url.QueryEscape(q.Get("state")) + "&error=access_denied")
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
	})
	if err == nil || err.Error() != "Google authorization was denied" {
		t.Fatalf("unexpected result: %v", err)
	}
}
func TestCredentialsRequireDesktopApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	os.WriteFile(path, []byte(`{"web":{"client_id":"wrong"}}`), 0600)
	if _, err := FromCredentials(path, "me@example.com"); err == nil {
		t.Fatal("accepted web client")
	}
	os.WriteFile(path, []byte(`{"installed":{"client_id":"desktop","client_secret":"secret"}}`), 0600)
	c, err := FromCredentials(path, "ME@example.com")
	if err != nil || c.ClientID != "desktop" || c.Email != "me@example.com" {
		t.Fatalf("credentials: %v %v", c, err)
	}
}

func TestBackendConfigCompatibility(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"email":"me@example.com","client_id":"old-client","client_secret":"secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil || c.BackendName() != "oauth" || c.ClientID != "old-client" {
		t.Fatalf("legacy config: %+v %v", c, err)
	}
	c = Config{Email: c.Email, Backend: "gws", GWSPath: "/opt/homebrew/bin/gws"}
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil || loaded != c {
		t.Fatalf("gws config: %+v %v", loaded, err)
	}
	stat, err := os.Stat(path)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("config permissions: %v %v", stat, err)
	}
}
