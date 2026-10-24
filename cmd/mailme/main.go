package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-smtp"
	"golang.org/x/oauth2"
	"mailme/internal/auth"
	"mailme/internal/bridge"
	"mailme/internal/gmail"
	"mailme/internal/gws"
	"mailme/internal/launchagent"
	"mailme/internal/localtls"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func defaultDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ".local/mailme"
	}
	return filepath.Join(dir, "mailme")
}
func run() error {
	if len(os.Args) < 2 {
		usage()
		return nil
	}
	command := os.Args[1]
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	dir := f.String("data-dir", defaultDir(), "Private configuration and mail cache directory")
	credentials := f.String("credentials", "", "Downloaded Google Desktop OAuth client JSON (login)")
	email := f.String("email", "", "Your Gmail/Workspace email (login)")
	backend := f.String("backend", "oauth", "Gmail backend: gws or oauth (login; saved for serve)")
	gwsPath := f.String("gws-path", "gws", "Installed gws executable (login --backend gws)")
	concurrency := f.Int("read-concurrency", 6, "Concurrent metadata reads / gws read subprocesses (1–16)")
	quotaProfile := f.String("quota-profile", "current", "Gmail quota profile: current (conservative default) or legacy (only if verified)")
	copyPassword := f.Bool("copy-password", false, "Copy the local password with pbcopy instead of printing it (settings)")
	probeID := f.String("message-id", "", "Probe one Gmail message ID (read-only probe)")
	probeStart := f.Int("start", 0, "First sorted message index (read-only probe)")
	probeCount := f.Int("count", 100, "Maximum messages to fetch (read-only probe)")
	imapAddr := f.String("imap", "127.0.0.1:1993", "Loopback IMAP TLS listener")
	smtpAddr := f.String("smtp", "127.0.0.1:1465", "Loopback SMTP TLS listener")
	interval := f.Duration("sync-interval", 30*time.Second, "Gmail history polling interval (minimum 10s)")
	if command == "help" || command == "--help" || command == "-h" {
		usage()
		return nil
	}
	if err := f.Parse(os.Args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *concurrency < 1 || *concurrency > 16 {
		return fmt.Errorf("--read-concurrency must be between 1 and 16")
	}
	if !gmail.ValidQuotaProfile(*quotaProfile) {
		return fmt.Errorf("--quota-profile must be current or legacy")
	}
	switch command {
	case "install-agent", "uninstall-agent":
		if runtime.GOOS != "darwin" {
			return fmt.Errorf("LaunchAgents require macOS")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		if command == "uninstall-agent" {
			return launchagent.Uninstall(ctx, home)
		}
		if *interval < 10*time.Second {
			return fmt.Errorf("--sync-interval must be at least 10s")
		}
		if _, err := auth.Load(*dir); err != nil {
			return fmt.Errorf("run login first: %w", err)
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		dataDir, err := filepath.Abs(*dir)
		if err != nil {
			return err
		}
		path, err := launchagent.Install(ctx, home, binary, dataDir, *imapAddr, *smtpAddr, interval.String(), *quotaProfile, *concurrency)
		if err != nil {
			return err
		}
		fmt.Printf("Installed %s\nLogs: %s\n", path, filepath.Join(home, "Library", "Logs", "mailme", "bridge.log"))
		return nil
	case "probe":
		if *probeStart < 0 || *probeCount < 1 {
			return fmt.Errorf("probe requires --start >= 0 and --count >= 1")
		}
		c, err := auth.Load(*dir)
		if err != nil {
			return err
		}
		api, err := newAPI(ctx, c, *concurrency, *quotaProfile)
		if err != nil {
			return err
		}
		log.Printf("Read-only probe quota profile: %s", *quotaProfile)
		return probe(ctx, api, *probeID, *probeStart, *probeCount, *concurrency)
	case "login":
		if *email == "" {
			return fmt.Errorf("login requires --email you@company.com and either --backend gws or --credentials /path/client.json")
		}
		a, err := mail.ParseAddress(*email)
		if err != nil || a.Address != *email {
			return fmt.Errorf("--email must be a plain email address")
		}
		if old, err := auth.Load(*dir); err == nil && !strings.EqualFold(old.Email, *email) {
			return fmt.Errorf("directory belongs to another account; choose a separate --data-dir")
		}
		var c auth.Config
		switch *backend {
		case "gws":
			if *credentials != "" {
				return fmt.Errorf("--credentials is for --backend oauth; gws manages its own credentials")
			}
			c, err = connectGWS(ctx, *gwsPath, *email)
		case "oauth":
			if *credentials == "" {
				return fmt.Errorf("OAuth login requires --credentials /path/client.json; use --backend gws for the installed CLI")
			}
			c, err = connectOAuth(ctx, *credentials, *email)
		default:
			return fmt.Errorf("unknown backend %q; choose gws or oauth", *backend)
		}
		if err != nil {
			return err
		}
		if _, err = auth.EnsurePassword(c.Email); err != nil {
			return err
		}
		if _, err = localtls.Ensure(*dir); err != nil {
			return err
		}
		if err = auth.Save(*dir, c); err != nil {
			return err
		}
		fmt.Printf("Gmail API connected using %s. Run `mailme settings --copy-password` for Apple Mail setup, then `mailme serve`.\n", c.BackendName())
		return nil
	case "settings":
		c, err := auth.Load(*dir)
		if err != nil {
			return err
		}
		password, err := auth.Password(c.Email)
		if err != nil {
			return err
		}
		if *copyPassword {
			cmd := exec.CommandContext(ctx, "pbcopy")
			cmd.Stdin = strings.NewReader(password)
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("could not copy local password with pbcopy")
			}
			password = "copied to clipboard (not printed)"
		}
		fmt.Printf("Gmail backend: %s\n", c.BackendName())
		if c.BackendName() == "gws" {
			fmt.Printf("gws executable: %s\n", c.GWSPath)
		}
		fmt.Printf("Apple Mail: Other Mail Account (IMAP)\nEmail / username: %s\nLocal bridge password: %s\nIncoming server: %s (TLS)\nOutgoing server: %s (TLS, Password authentication)\nCertificate to trust: %s\nDrafts and client-saved Sent: On My Mac\nDisable automatic connection settings; select the ports shown above.\n", c.Email, password, *imapAddr, *smtpAddr, filepath.Join(*dir, "localhost.crt"))
		return nil
	case "serve":
		if *interval < 10*time.Second {
			return fmt.Errorf("--sync-interval must be at least 10s")
		}
		c, err := auth.Load(*dir)
		if err != nil {
			return fmt.Errorf("run login first: %w", err)
		}
		api, err := newAPI(ctx, c, *concurrency, *quotaProfile)
		if err != nil {
			return err
		}
		password, err := auth.Password(c.Email)
		if err != nil {
			return err
		}
		b, err := bridge.Open(*dir, c.Email, password, api, bridge.Options{Workers: *concurrency})
		if err != nil {
			return err
		}
		defer b.Close()
		tlsConfig, err := localtls.Ensure(*dir)
		if err != nil {
			return err
		}
		imapLn, err := localtls.Listen(*imapAddr, tlsConfig)
		if err != nil {
			return err
		}
		defer imapLn.Close()
		smtpLn, err := localtls.Listen(*smtpAddr, tlsConfig)
		if err != nil {
			return err
		}
		defer smtpLn.Close()
		im := imapserver.New(&imapserver.Options{NewSession: func(_ *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return b.NewSession(), nil, nil
		}, Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}, imap.CapSpecialUse: {}, imap.CapUnselect: {}}, TLSConfig: tlsConfig})
		sm := smtp.NewServer(b.SMTPBackend())
		sm.Domain = "localhost"
		sm.MaxMessageBytes = bridge.MaxMessageBytes
		sm.MaxRecipients = 100
		sm.ReadTimeout = 5 * time.Minute
		sm.WriteTimeout = time.Minute
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); b.RunSync(ctx, *interval) }()
		defer func() { cancel(); b.Stop(); im.Close(); sm.Close(); wg.Wait() }()
		errors := make(chan error, 2)
		go func() { errors <- im.Serve(imapLn) }()
		go func() { errors <- sm.Serve(smtpLn) }()
		log.Printf("Ready: IMAP TLS %s; SMTP TLS %s; serving cached/partial mail while Gmail sync runs (%d readers, %s quota)", *imapAddr, *smtpAddr, *concurrency, *quotaProfile)
		select {
		case <-ctx.Done():
			return nil
		case err := <-errors:
			return err
		}
	default:
		return fmt.Errorf("unknown command %q; run mailme help", command)
	}
}

func connectGWS(ctx context.Context, path, email string) (auth.Config, error) {
	c := auth.Config{Email: strings.ToLower(email), Backend: "gws"}
	api, err := gws.New(path, c.Email)
	if err != nil {
		return auth.Config{}, err
	}
	if _, err = api.Profile(ctx); err != nil {
		return auth.Config{}, fmt.Errorf("gws account check failed (authenticate the CLI for this account with Gmail modify access): %w", err)
	}
	c.GWSPath = api.Path()
	return c, nil
}

func connectOAuth(ctx context.Context, credentials, email string) (auth.Config, error) {
	c, err := auth.FromCredentials(credentials, email)
	if err != nil {
		return c, err
	}
	loginCtx, stop := context.WithTimeout(ctx, 5*time.Minute)
	defer stop()
	token, err := auth.Authorize(loginCtx, c, func(url string) {
		fmt.Printf("Authorize this Gmail API app in your browser:\n%s\n", url)
		if runtime.GOOS == "darwin" {
			_ = exec.Command("open", url).Run()
		}
	})
	if err != nil {
		return c, err
	}
	client := c.OAuth().Client(context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 30 * time.Second}), token)
	client.Timeout = 30 * time.Second
	profile, err := (&gmail.Client{HTTP: client, Email: c.Email}).Profile(ctx)
	if err != nil {
		return c, err
	}
	if !strings.EqualFold(profile.Email, c.Email) {
		return c, fmt.Errorf("authorized account does not match --email; nothing saved")
	}
	return c, auth.SaveToken(c.Email, token)
}

func newAPI(ctx context.Context, c auth.Config, concurrency int, quotaProfile string) (bridge.API, error) {
	if !gmail.ValidQuotaProfile(quotaProfile) {
		return nil, fmt.Errorf("quota profile must be current or legacy")
	}
	switch c.BackendName() {
	case "gws":
		client, err := gws.New(c.GWSPath, c.Email, concurrency)
		if err != nil {
			return nil, err
		}
		if err = client.SetQuotaProfile(quotaProfile); err != nil {
			return nil, err
		}
		return client, nil
	case "oauth":
		httpClient, err := auth.Client(ctx, c)
		if err != nil {
			return nil, err
		}
		return &gmail.Client{HTTP: httpClient, Email: c.Email, QuotaProfile: quotaProfile}, nil
	default:
		return nil, fmt.Errorf("unknown saved backend %q; run login with --backend gws or --backend oauth", c.Backend)
	}
}

func usage() {
	fmt.Println(`Mailme — local Apple Mail ↔ Gmail API bridge

Commands:
  login     Connect an authenticated gws CLI or a Google Desktop OAuth app
  settings  Show Apple Mail setup; --copy-password avoids printing the password
  serve     Synchronize Gmail and serve IMAP + SMTP over TLS on loopback
  probe     Read-only metadata diagnostic and throughput measurement
  install-agent    Install and load the per-user macOS LaunchAgent
  uninstall-agent  Unload and remove the LaunchAgent (keeps mail data)

Start:
  mailme login --backend gws --email you@company.com
  mailme settings --copy-password
  mailme serve

Or use a Desktop OAuth app:
  mailme login --credentials /path/client.json --email you@company.com

Use -h after a command for flags. The bridge never connects to Gmail IMAP/SMTP.`)
}
