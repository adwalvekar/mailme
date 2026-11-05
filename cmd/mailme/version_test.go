package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestReleaseVersionMetadata(t *testing.T) {
	previousVersion, previousCommit, previousDate := version, commit, buildDate
	t.Cleanup(func() { version, commit, buildDate = previousVersion, previousCommit, previousDate })
	version, commit, buildDate = "v1.2.3-rc.1", "0123456789abcdef", "2026-11-05T00:00:00Z"
	var output bytes.Buffer
	if err := printVersion(&output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{version, commit, buildDate, "mailme "} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing version metadata %q: %s", want, output.String())
		}
	}
}
