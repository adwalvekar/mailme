package launchagent

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"

	"mailme/internal/settings"
)

// Do not call Install/Uninstall or launchctl in tests.
func TestRenderOnly(t *testing.T) {
	tuning := settings.Default()
	tuning.Request.ReadRetries = 0
	data, err := Render("/opt/Mail & Co/mailme", "/Users/test/Library/Application Support/mailme", "/Users/test/logs/mailme.log", "127.0.0.1:1993", "127.0.0.1:1465", tuning)
	if err != nil {
		t.Fatal(err)
	}
	args := tuning.Args()
	for i := 0; i < len(args); i += 2 {
		if !strings.Contains(string(data), "<string>"+args[i]+"</string>\n<string>"+args[i+1]+"</string>") {
			t.Fatalf("service lost setting %s=%s", args[i], args[i+1])
		}
	}
	if !strings.Contains(string(data), "Mail &amp; Co") || !strings.Contains(string(data), "--read-concurrency") || !strings.Contains(string(data), "<string>--quota-profile</string>\n<string>current</string>") || strings.Contains(string(data), "password") {
		t.Fatal("invalid service arguments")
	}
	d := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		_, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Render("relative", "/data", "/log", "", "", tuning); err == nil {
		t.Fatal("relative executable accepted")
	}
}
