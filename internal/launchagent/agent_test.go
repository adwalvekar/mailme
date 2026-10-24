package launchagent

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

// Do not call Install/Uninstall or launchctl in tests.
func TestRenderOnly(t *testing.T) {
	data, err := Render("/opt/Mail & Co/mailme", "/Users/test/Library/Application Support/mailme", "/Users/test/logs/mailme.log", "127.0.0.1:1993", "127.0.0.1:1465", "30s", "current", 6)
	if err != nil {
		t.Fatal(err)
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
	if _, err := Render("relative", "/data", "/log", "", "", "30s", "current", 6); err == nil {
		t.Fatal("relative executable accepted")
	}
}
