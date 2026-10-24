// Package launchagent installs an explicitly requested per-user macOS service.
package launchagent

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

const Label = "com.mailme.bridge"

// Render is pure: tests can verify the plan without invoking launchctl.
func Render(binary, dataDir, logPath, imap, smtp, interval, quotaProfile string, concurrency int) ([]byte, error) {
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
	for _, arg := range []string{binary, "serve", "--data-dir", dataDir, "--imap", imap, "--smtp", smtp, "--sync-interval", interval, "--quota-profile", quotaProfile, "--read-concurrency", strconv.Itoa(concurrency)} {
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
	value("string", logPath)
	value("key", "StandardErrorPath")
	value("string", logPath)
	b.WriteString("</dict></plist>\n")
	return b.Bytes(), nil
}

func location(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

func Install(ctx context.Context, home, binary, dataDir, imap, smtp, interval, quotaProfile string, concurrency int) (string, error) {
	logDir := filepath.Join(home, "Library", "Logs", "mailme")
	path := location(home)
	data, err := Render(binary, dataDir, filepath.Join(logDir, "bridge.log"), imap, smtp, interval, quotaProfile, concurrency)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(logDir, 0700); err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "bridge.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	logFile.Close()
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
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if exec.CommandContext(ctx, "launchctl", "print", domain+"/"+Label).Run() == nil {
		if err = exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+Label).Run(); err != nil {
			return path, fmt.Errorf("could not unload existing LaunchAgent: %w", err)
		}
	}
	if err = exec.CommandContext(ctx, "launchctl", "bootstrap", domain, path).Run(); err != nil {
		return path, fmt.Errorf("LaunchAgent saved but bootstrap failed: %w", err)
	}
	return path, nil
}

func Uninstall(ctx context.Context, home string) error {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if exec.CommandContext(ctx, "launchctl", "print", domain+"/"+Label).Run() == nil {
		if err := exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+Label).Run(); err != nil {
			return fmt.Errorf("could not unload LaunchAgent: %w", err)
		}
	}
	err := os.Remove(location(home))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
