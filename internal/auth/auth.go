// Package auth manages desktop OAuth with PKCE and macOS Keychain secrets.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
)

const Scope = "https://www.googleapis.com/auth/gmail.modify"
const service = "mailme.gmail-api-bridge"

type Config struct {
	Email        string `json:"email"`
	Backend      string `json:"backend,omitempty"`
	GWSPath      string `json:"gws_path,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
}

// BackendName preserves configurations created before backend selection existed.
func (c Config) BackendName() string {
	if c.Backend == "" {
		return "oauth"
	}
	return c.Backend
}

func (c Config) OAuth() *oauth2.Config {
	return &oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Scopes: []string{Scope}, Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token"}}
}
func Load(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}
func Save(dir string, c Config) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(dir, "config.json"), b)
}
func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".mailme-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func FromCredentials(path, email string) (Config, error) {
	var data struct {
		Installed struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		} `json:"installed"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	if err = json.Unmarshal(b, &data); err != nil {
		return Config{}, err
	}
	if data.Installed.ClientID == "" {
		return Config{}, errors.New("credentials must be a Google Desktop app OAuth client")
	}
	return Config{Email: strings.ToLower(email), ClientID: data.Installed.ClientID, ClientSecret: data.Installed.ClientSecret}, nil
}
func RandomSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func Password(email string) (string, error) { return keyring.Get(service, email+":bridge") }
func EnsurePassword(email string) (string, error) {
	p, err := Password(email)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, keyring.ErrNotFound) {
		return "", err
	}
	p = RandomSecret()
	return p, keyring.Set(service, email+":bridge", p)
}
func SaveToken(email string, t *oauth2.Token) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return keyring.Set(service, email+":oauth", string(b))
}
func Client(ctx context.Context, c Config) (*http.Client, error) {
	value, err := keyring.Get(service, c.Email+":oauth")
	if err != nil {
		return nil, fmt.Errorf("read OAuth token from Keychain (run login first): %w", err)
	}
	var token oauth2.Token
	if err = json.Unmarshal([]byte(value), &token); err != nil {
		return nil, err
	}
	// Refresh requests need a timeout too, independently of Gmail API requests.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 30 * time.Second})
	source := &persistSource{source: c.OAuth().TokenSource(ctx, &token), email: c.Email, last: token.AccessToken, refresh: token.RefreshToken}
	return &http.Client{Transport: &oauth2.Transport{Source: source}, Timeout: 60 * time.Second}, nil
}

type persistSource struct {
	mu                   sync.Mutex
	source               oauth2.TokenSource
	email, last, refresh string
}

func (s *persistSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.source.Token()
	if err != nil {
		return nil, errors.New("Google OAuth refresh failed; run login again if access was revoked")
	}
	if t.RefreshToken == "" {
		copy := *t
		copy.RefreshToken = s.refresh
		t = &copy
	}
	if t.AccessToken != s.last || t.RefreshToken != s.refresh {
		if err = SaveToken(s.email, t); err != nil {
			return nil, err
		}
		s.last = t.AccessToken
		s.refresh = t.RefreshToken
	}
	return t, nil
}

// Authorize uses an ephemeral IPv4 loopback redirect, a random state, and PKCE.
// The caller displays/opens the URL; no browser cookies or passwords are read.
func Authorize(ctx context.Context, c Config, show func(string)) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	o := c.OAuth()
	o.RedirectURL = "http://" + ln.Addr().String() + "/oauth/callback"
	state, verifier := RandomSecret(), oauth2.GenerateVerifier()
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method != "GET" || r.URL.Query().Get("state") != state {
			http.Error(w, "Invalid OAuth state", 400)
			return
		}
		var res result
		if r.URL.Query().Get("error") != "" {
			res.err = errors.New("Google authorization was denied")
		} else {
			res.code = r.URL.Query().Get("code")
			if res.code == "" {
				http.Error(w, "Missing authorization code", 400)
				return
			}
		}
		select {
		case done <- res:
			fmt.Fprintln(w, "Authorization received. Return to the terminal.")
		default:
			http.Error(w, "Callback already received", 409)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	defer server.Close()
	go server.Serve(ln)
	show(o.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "consent"), oauth2.SetAuthURLParam("login_hint", c.Email)))
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 30 * time.Second})
		t, err := o.Exchange(ctx, r.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, errors.New("Google token exchange failed")
		}
		if t.RefreshToken == "" {
			return nil, errors.New("Google did not issue a refresh token; revoke app access and authorize again")
		}
		return t, nil
	}
}
