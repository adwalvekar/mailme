package gmail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSafeErrorClassification(t *testing.T) {
	e := ParseAPIError([]byte(`{"error":{"code":400,"status":"FAILED_PRECONDITION","message":"PRIVATE CONTENT","errors":[{"reason":"failedPrecondition"},{"reason":"PRIVATE CONTENT"}]}}`), 0)
	e.Method, e.ID = "messages.get", "abc123"
	if !PerMessage(e) || strings.Contains(e.Error(), "PRIVATE") || !strings.Contains(e.Error(), "FAILED_PRECONDITION") {
		t.Fatal(e)
	}
	if PerMessage(&APIError{Status: 400}) || PerMessage(&APIError{Status: 403, Reasons: []string{"forbidden"}}) || PerMessage(&APIError{Status: 401}) {
		t.Fatal("systemic error treated as bad message")
	}
	if !PerMessage(&APIError{Status: 404}) || !PerMessage(&MalformedMessageError{ID: "a"}) {
		t.Fatal("per-message error lost")
	}
	if RetryDelay("120", time.Now()) != time.Minute || RetryDelay(time.Now().Add(5*time.Second).UTC().Format(http.TimeFormat), time.Now()) < 3*time.Second {
		t.Fatal("Retry-After parsing")
	}
}

func TestDirect403RetryAndMetadataProjection(t *testing.T) {
	var count atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"code":403,"errors":[{"reason":"rateLimitExceeded"}],"message":"PRIVATE"}}`)
			return
		}
		if r.URL.Query().Get("fields") != MetadataFields || len(r.URL.Query()["metadataHeaders"]) != len(MetadataHeaders) {
			t.Error("missing metadata projection")
		}
		fmt.Fprint(w, `{"id":"a","internalDate":"123","labelIds":["INBOX"],"payload":{"headers":[{"name":"Subject","value":"test"}]}}`)
	}))
	defer s.Close()
	c := &Client{HTTP: s.Client(), BaseURL: s.URL}
	start := time.Now()
	m, err := c.Message(context.Background(), "a", "metadata")
	if err != nil || m.Payload == nil || count.Load() != 2 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("retry/projection: %v %v", m, err)
	}
	count.Store(0)
	if _, err = c.Modify(context.Background(), "a", nil, []string{"UNREAD"}); err == nil || count.Load() != 1 {
		t.Fatalf("mutation retried: %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.ID != "a" || apiErr.Method != "messages.modify" {
		t.Fatal(err)
	}
}
