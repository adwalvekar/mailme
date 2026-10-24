package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"

	"mailme/internal/gmail"
)

type probeFake struct {
	mu     sync.Mutex
	listed bool
	reads  []string
}

func (f *probeFake) Profile(context.Context) (*gmail.Profile, error) {
	return &gmail.Profile{Email: "me@example.com", HistoryID: "10"}, nil
}
func (f *probeFake) IDs(context.Context) ([]string, error) {
	f.listed = true
	return []string{"c", "a", "b"}, nil
}
func (f *probeFake) Message(_ context.Context, id, format string) (*gmail.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, id+":"+format)
	return nil, &gmail.APIError{Status: 400, Method: "messages.get", ID: id, Reasons: []string{"failedPrecondition"}}
}

func TestProbeSortedRangeAndSafeDiagnostics(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	f := &probeFake{}
	if err := probe(context.Background(), f, "", 1, 1, 2); err == nil {
		t.Fatal("failed read was not reported")
	}
	if !f.listed || len(f.reads) != 1 || f.reads[0] != "b:metadata" {
		t.Fatalf("wrong probe range or format: %v", f.reads)
	}
	for _, want := range []string{"index=1 id=b", "failedPrecondition", "failed=1"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing diagnostic %q", want)
		}
	}
	f = &probeFake{}
	_ = probe(context.Background(), f, "c", 0, 1, 1)
	if f.listed || len(f.reads) != 1 || f.reads[0] != "c:metadata" {
		t.Fatalf("single-message probe listed mailbox: %v", f.reads)
	}
}
