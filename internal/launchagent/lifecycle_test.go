package launchagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailme/internal/settings"
)

func TestStartIsIdempotentAndStopKeepsAutoStart(t *testing.T) {
	home := t.TempDir()
	loaded := false
	var commands []string
	m := Manager{Home: home, UID: 501, Run: func(ctx context.Context, args ...string) ([]byte, error) {
		commands = append(commands, args[0])
		switch args[0] {
		case "print":
			if !loaded {
				return nil, errNotLoaded
			}
			return []byte("service = {\n\tstate = running\n\tpid = 321\n\tlast exit code = 0\n\tenvironment = { PRIVATE = SECRET }\n}"), nil
		case "bootstrap":
			loaded = true
		case "bootout":
			loaded = false
		default:
			t.Fatalf("unexpected command %v", args)
		}
		return nil, nil
	}}
	tuning := settings.Default()
	tuning.Logs.File = filepath.Join(home, "custom", "bridge.log")
	start := func(force bool) {
		t.Helper()
		if _, err := m.Start(context.Background(), "/opt/mailme", "/data/mailme", "127.0.0.1:1993", "127.0.0.1:1465", tuning, force); err != nil {
			t.Fatal(err)
		}
	}
	start(false)
	if !reflect.DeepEqual(commands, []string{"print", "bootstrap"}) {
		t.Fatalf("first start: %v", commands)
	}
	commands = nil
	start(false)
	if !reflect.DeepEqual(commands, []string{"print"}) {
		t.Fatalf("repeated start reloaded service: %v", commands)
	}
	status, err := m.Status(context.Background())
	if err != nil || !status.Installed || !status.Loaded || status.PID != 321 || status.State != "running" || status.LastExit == nil || *status.LastExit != 0 {
		t.Fatalf("bad status: %+v %v", status, err)
	}
	if path, err := m.LogPath(); err != nil || path != tuning.Logs.File {
		t.Fatalf("custom service log path lost: %s %v", path, err)
	}
	commands = nil
	start(true)
	if !reflect.DeepEqual(commands, []string{"print", "bootout", "bootstrap"}) {
		t.Fatalf("explicit install did not reload: %v", commands)
	}
	if err := m.Stop(context.Background()); err != nil || loaded {
		t.Fatalf("stop failed: %v", err)
	}
	if _, err := os.Stat(location(home)); err != nil {
		t.Fatal("stop removed auto-start definition")
	}
	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(location(home)); !os.IsNotExist(err) {
		t.Fatal("uninstall retained service definition")
	}
}

func TestServiceFailuresAreReported(t *testing.T) {
	for _, operation := range []string{"print", "bootstrap", "bootout"} {
		t.Run(operation, func(t *testing.T) {
			m := Manager{Home: t.TempDir(), UID: 501, Run: func(ctx context.Context, args ...string) ([]byte, error) {
				if args[0] == operation {
					return nil, errors.New("simulated launchctl failure")
				}
				if args[0] == "print" {
					if operation == "bootout" {
						return []byte("state = running"), nil
					}
					return nil, errNotLoaded
				}
				return nil, nil
			}}
			tuning := settings.Default()
			tuning.Logs.File = filepath.Join(m.Home, "logs", "bridge.log")
			if _, err := m.Start(context.Background(), "/opt/mailme", "/data", "", "", tuning, true); err == nil {
				t.Fatalf("%s failure reported success", operation)
			}
		})
	}
}

func TestStatusDoesNotExposeEnvironmentAndReportsNonzeroExit(t *testing.T) {
	m := Manager{Home: t.TempDir(), UID: 501, Run: func(context.Context, ...string) ([]byte, error) {
		return []byte("state = spawn scheduled\nlast exit code = 1\nenvironment = { PRIVATE = SECRET }"), nil
	}}
	status, err := m.Status(context.Background())
	if err != nil || status.State != "spawn scheduled" || status.PID != 0 || status.LastExit == nil || *status.LastExit != 1 {
		t.Fatalf("failed start hidden: %+v %v", status, err)
	}
	if strings.Contains(status.State, "SECRET") {
		t.Fatal("status exposed environment")
	}
}

func TestChangedSettingsReloadAndPreserveForeground(t *testing.T) {
	m := Manager{Home: t.TempDir(), UID: 501}
	var calls []string
	loaded := false
	m.Run = func(ctx context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args[0])
		if args[0] == "print" && !loaded {
			return nil, errNotLoaded
		}
		if args[0] == "bootstrap" {
			loaded = true
		}
		return []byte("state = running"), nil
	}
	tuning := settings.Default()
	tuning.Logs.File = filepath.Join(m.Home, "logs", "bridge.log")
	if _, err := m.Start(context.Background(), "/opt/mailme", "/data", "", "", tuning, false); err != nil {
		t.Fatal(err)
	}
	calls = nil
	tuning.Logs.MaxSizeMB = 2
	if _, err := m.Start(context.Background(), "/opt/mailme", "/data", "", "", tuning, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"print", "bootout", "bootstrap"}) {
		t.Fatalf("updated settings not loaded: %v", calls)
	}
	data, _ := os.ReadFile(location(m.Home))
	if !strings.Contains(string(data), "<string>--foreground</string>") || !strings.Contains(string(data), "<string>--log-max-size</string>\n<string>2</string>") {
		t.Fatal("recursive background start or settings lost")
	}
}
