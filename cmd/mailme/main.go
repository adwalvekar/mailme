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
	switch command {
	case "login":
		if *credentials == "" || *email == "" {
			return fmt.Errorf("usage: mailme login --credentials /path/client.json --email you@company.com")
		}
		a, err := mail.ParseAddress(*email)
		if err != nil || a.Address != *email {
			return fmt.Errorf("--email must be a plain email address")
		}
		if old, err := auth.Load(*dir); err == nil && !strings.EqualFold(old.Email, *email) {
			return fmt.Errorf("directory belongs to another account; choose a separate --data-dir")
		}
		c, err := auth.FromCredentials(*credentials, *email)
		if err != nil {
			return err
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
			return err
		}
		client := c.OAuth().Client(context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 30 * time.Second}), token)
		client.Timeout = 30 * time.Second
		profile, err := (&gmail.Client{HTTP: client}).Profile(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(profile.Email, c.Email) {
			return fmt.Errorf("authorized account does not match --email; nothing saved")
		}
		if err = auth.SaveToken(c.Email, token); err != nil {
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
		fmt.Println("Gmail API connected. Run `mailme settings` for Apple Mail setup, then `mailme serve`.")
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
		httpClient, err := auth.Client(ctx, c)
		if err != nil {
			return err
		}
		password, err := auth.Password(c.Email)
		if err != nil {
			return err
		}
		api := &gmail.Client{HTTP: httpClient}
		b, err := bridge.Open(*dir, c.Email, password, api)
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
		log.Print("Synchronizing Gmail metadata; first run can take several minutes for large mailboxes")
		if err = b.Sync(ctx); err != nil {
			return fmt.Errorf("initial Gmail synchronization failed: %w", err)
		}
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
		log.Printf("Ready: IMAP TLS %s; SMTP TLS %s; Gmail API polling every %s", *imapAddr, *smtpAddr, *interval)
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
func usage() {
	fmt.Println(`Mailme — local Apple Mail ↔ Gmail API bridge

Commands:
  login     Connect a Google Desktop OAuth app; store credentials in Keychain
  settings  Show Apple Mail ports, certificate path, and local bridge password
  serve     Synchronize Gmail and serve IMAP + SMTP over TLS on loopback

Start:
  mailme login --credentials /path/client.json --email you@company.com
  mailme settings
  mailme serve

Use -h after a command for flags. The bridge never connects to Gmail IMAP/SMTP.`)
}
