package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestPaginationAndRawSend(t *testing.T) {
	var sends atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/messages":
			if r.URL.Query().Get("includeSpamTrash") != "true" {
				t.Error("spam/trash omitted")
			}
			if r.URL.Query().Get("pageToken") == "" {
				fmt.Fprint(w, `{"messages":[{"id":"a"}],"nextPageToken":"page+2"}`)
			} else if r.URL.Query().Get("pageToken") == "page+2" {
				fmt.Fprint(w, `{"messages":[{"id":"b"}]}`)
			} else {
				t.Error("bad pagination token")
			}
		case "/history":
			if r.URL.Query().Get("startHistoryId") != "123" {
				t.Error("checkpoint missing")
			}
			if r.URL.Query().Get("pageToken") == "" {
				fmt.Fprint(w, `{"history":[{"messages":[{"id":"a"}]}],"nextPageToken":"next","historyId":"124"}`)
			} else {
				fmt.Fprint(w, `{"history":[{"messages":[{"id":"a"},{"id":"b"}]}],"historyId":"130"}`)
			}
		case "/messages/send":
			sends.Add(1)
			var body struct {
				Raw string `json:"raw"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			raw, err := base64.RawURLEncoding.DecodeString(body.Raw)
			if err != nil || string(raw) != "MIME bytes\r\n\x00" {
				t.Error("raw MIME encoding changed")
			}
			fmt.Fprint(w, `{"id":"sent"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	c := &Client{HTTP: s.Client(), BaseURL: s.URL}
	ctx := context.Background()
	ids, err := c.IDs(ctx)
	if err != nil || !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("IDs: %v %v", ids, err)
	}
	changes, cursor, err := c.Changes(ctx, "123")
	if err != nil || len(changes) != 2 || cursor != "130" {
		t.Fatalf("changes: %v %s %v", changes, cursor, err)
	}
	if err = c.Send(ctx, []byte("MIME bytes\r\n\x00")); err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 1 {
		t.Fatal("send count")
	}
}
func TestSendNeverRetriesAndErrorsRedactBody(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "PRIVATE MESSAGE CONTENT", 503)
	}))
	defer s.Close()
	c := &Client{HTTP: s.Client(), BaseURL: s.URL}
	err := c.Send(context.Background(), []byte("test"))
	if err == nil || err.Error() != "Gmail API returned HTTP 503" || calls.Load() != 1 {
		t.Fatalf("send result: %v, calls=%d", err, calls.Load())
	}
}
func TestReadRetryAndCancellation(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "busy", 503)
			return
		}
		fmt.Fprint(w, `{"emailAddress":"me@example.com","historyId":"123"}`)
	}))
	defer s.Close()
	c := &Client{HTTP: s.Client(), BaseURL: s.URL}
	p, err := c.Profile(context.Background())
	if err != nil || p.HistoryID != "123" || calls.Load() != 2 {
		t.Fatalf("retry: %v %v", p, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.next = time.Now().Add(time.Minute)
	if _, err = c.Profile(ctx); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}
