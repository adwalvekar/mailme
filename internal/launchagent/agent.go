// Package launchagent manages the per-user macOS background service.
package launchagent

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"mailme/internal/settings"
)

const Label = "com.mailme.bridge"

// Render is pure: tests can verify the plan without invoking launchctl.
func Render(binary, dataDir, logPath, imap, smtp string, tuning settings.Runtime) ([]byte, error) {
	tuning.Logs.File = logPath
	tuning.Logs.Stderr = false // launchd stdio is discarded; the app owns rotation
	if err := tuning.Validate(); err != nil {
		return nil, err
	}
	for _, path := range []string{binary, dataDir, logPath} {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("LaunchAgent paths must be absolute")
		}
	}
	var b bytes.Buffer
	b.WriteString(xml.Header + "<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n")
	value := func(tag, s string) {
		b.WriteString("<" + tag + ">")
		xml.EscapeText(&b, []byte(s))
		b.WriteString("</" + tag + ">\n")
	}
	value("key", "Label")
	value("string", Label)
	value("key", "ProgramArguments")
	b.WriteString("<array>\n")
	args := append([]string{binary, "serve", "--foreground", "--data-dir", dataDir, "--imap", imap, "--smtp", smtp}, tuning.Args()...)
	for _, arg := range args {
		value("string", arg)
	}
	b.WriteString("</array>\n")
	value("key", "RunAtLoad")
	b.WriteString("<true/>\n")
	value("key", "KeepAlive")
	b.WriteString("<true/>\n")
	value("key", "ThrottleInterval")
	b.WriteString("<integer>30</integer>\n")
	value("key", "StandardOutPath")
	value("string", "/dev/null")
	value("key", "StandardErrorPath")
	value("string", "/dev/null")
	b.WriteString("</dict></plist>\n")
	return b.Bytes(), nil
}

func location(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

// Manager's command runner is replaceable so tests never load a real service.
type Manager struct {
	Home string
	UID  int
	Run  func(context.Context, ...string) ([]byte, error)
}

func New(home string) Manager { return Manager{Home: home, UID: os.Getuid(), Run: runLaunchctl} }

func (m Manager) target() string { return "gui/" + strconv.Itoa(m.UID) + "/" + Label }

// Start is idempotent for an already loaded, identically configured service.
// force is used by install-agent to explicitly reload even an unchanged plist.
func (m Manager) Start(ctx context.Context, binary, dataDir, imap, smtp string, tuning settings.Runtime, force bool) (string, error) {
	path := location(m.Home)
	data, err := Render(binary, dataDir, tuning.Logs.File, imap, smtp, tuning)
	if err != nil {
		return "", err
	}
	status, err := m.Status(ctx)
	if err != nil {
		return "", err
	}
	old, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", readErr
	}
	if !force && status.Loaded && bytes.Equal(old, data) {
		return path, nil
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".mailme-agent-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	domain := "gui/" + strconv.Itoa(m.UID)
	if status.Loaded {
		if _, err = m.Run(ctx, "bootout", m.target()); err != nil {
			return path, fmt.Errorf("could not unload existing LaunchAgent: %w", err)
		}
	}
	if _, err = m.Run(ctx, "bootstrap", domain, path); err != nil {
		return path, fmt.Errorf("LaunchAgent saved but bootstrap failed: %w", err)
	}
	return path, nil
}

func (m Manager) Stop(ctx context.Context) error {
	status, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if status.Loaded {
		if _, err := m.Run(ctx, "bootout", m.target()); err != nil {
			return fmt.Errorf("could not unload LaunchAgent: %w", err)
		}
	}
	return nil
}

func (m Manager) Uninstall(ctx context.Context) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	err := os.Remove(location(m.Home))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

type Status struct {
	Installed bool
	Loaded    bool
	State     string
	PID       int
	LastExit  *int
}

func (m Manager) Status(ctx context.Context) (Status, error) {
	var status Status
	_, err := os.Stat(location(m.Home))
	if err != nil && !os.IsNotExist(err) {
		return status, err
	}
	status.Installed = err == nil
	data, err := m.Run(ctx, "print", m.target())
	if err != nil {
		if errors.Is(err, errNotLoaded) {
			return status, nil
		}
		return status, err
	}
	status.Loaded = true
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "state":
			if status.State == "" && len(value) < 64 && strings.Trim(value, "abcdefghijklmnopqrstuvwxyz ") == "" {
				status.State = value
			}
		case "pid":
			status.PID, _ = strconv.Atoi(value)
		case "last exit code":
			if code, err := strconv.Atoi(value); err == nil {
				status.LastExit = &code
			}
		}
	}
	return status, nil
}

// LogPath reads saved service arguments without invoking launchctl or credentials.
func (m Manager) LogPath() (string, error) {
	data, err := os.ReadFile(location(m.Home))
	if err != nil {
		return "", err
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	var values []string
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if element, ok := token.(xml.StartElement); ok && element.Name.Local == "string" {
			var value string
			if err := d.DecodeElement(&value, &element); err != nil {
				return "", err
			}
			values = append(values, value)
		}
	}
	for i, arg := range values {
		if arg == "--log-file" && i+1 < len(values) && filepath.IsAbs(values[i+1]) {
			return values[i+1], nil
		}
	}
	return "", nil // older plist: use the historical default log path
}

var errNotLoaded = errors.New("service is not loaded")

func runLaunchctl(ctx context.Context, args ...string) ([]byte, error) {
	var output limitedOutput
	cmd := exec.CommandContext(ctx, "launchctl", args...)
	cmd.Stdout = &output
	cmd.Stderr = io.Discard // never echo service environments or free-form diagnostics
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if args[0] == "print" && errors.As(err, &exit) && exit.ExitCode() == 113 {
			return nil, errNotLoaded
		}
		return nil, fmt.Errorf("launchctl %s failed: %w", args[0], err)
	}
	return output.Bytes(), nil
}

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 64<<10 - b.Len(); remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(remaining, n)])
	}
	return n, nil
}
