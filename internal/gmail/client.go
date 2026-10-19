// Package gmail is the bridge's only mail transport. It uses HTTPS, never IMAP or SMTP.
package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const Endpoint = "https://gmail.googleapis.com/gmail/v1/users/me"

type Message struct {
	ID           string   `json:"id"`
	LabelIDs     []string `json:"labelIds"`
	InternalDate string   `json:"internalDate"`
	Raw          string   `json:"raw,omitempty"`
}
type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}
type Profile struct {
	Email     string `json:"emailAddress"`
	HistoryID string `json:"historyId"`
}
type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("Gmail API returned HTTP %d", e.Status) }

type Client struct {
	HTTP    *http.Client
	BaseURL string // Override only in tests.
	mu      sync.Mutex
	next    time.Time
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	base := c.BaseURL
	if base == "" {
		base = Endpoint
	}
	var body []byte
	var err error
	if in != nil {
		body, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	// Pace requests below Gmail's per-user quota. Only GET requests are retried;
	// an ambiguous send outcome must never cause an automatic duplicate send.
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		when := c.next
		if when.Before(time.Now()) {
			when = time.Now()
		}
		c.next = when.Add(40 * time.Millisecond)
		c.mu.Unlock()
		if err := wait(ctx, time.Until(when)); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := c.HTTP.Do(req)
		if err != nil {
			return fmt.Errorf("Gmail API transport failed: %w", err)
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			if out != nil {
				err = json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(out)
			}
			res.Body.Close()
			return err
		}
		// Do not include Google's response body: it may contain message data.
		res.Body.Close()
		if method != http.MethodGet || attempt >= 3 || (res.StatusCode != 429 && res.StatusCode < 500) {
			return &APIError{Status: res.StatusCode}
		}
		delay := time.Second << attempt
		if secs, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && secs > 0 && secs < 60 {
			delay = time.Duration(secs) * time.Second
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
}
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (c *Client) Profile(ctx context.Context) (*Profile, error) {
	var p Profile
	err := c.call(ctx, "GET", "/profile", nil, &p)
	return &p, err
}
func (c *Client) Labels(ctx context.Context) ([]Label, error) {
	var r struct {
		Labels []Label `json:"labels"`
	}
	err := c.call(ctx, "GET", "/labels", nil, &r)
	return r.Labels, err
}
func (c *Client) IDs(ctx context.Context) ([]string, error) {
	var ids []string
	page := ""
	for {
		var r struct {
			Messages []Message `json:"messages"`
			Next     string    `json:"nextPageToken"`
		}
		err := c.call(ctx, "GET", "/messages?includeSpamTrash=true&maxResults=500&pageToken="+url.QueryEscape(page), nil, &r)
		if err != nil {
			return nil, err
		}
		for _, m := range r.Messages {
			ids = append(ids, m.ID)
		}
		page = r.Next
		if page == "" {
			return ids, nil
		}
	}
}
func (c *Client) Message(ctx context.Context, id, format string) (*Message, error) {
	var m Message
	err := c.call(ctx, "GET", "/messages/"+url.PathEscape(id)+"?format="+url.QueryEscape(format), nil, &m)
	return &m, err
}

// Changes gathers every history page before returning its checkpoint. Re-fetching
// affected IDs gives current state even when a message changed multiple times.
func (c *Client) Changes(ctx context.Context, start string) ([]string, string, error) {
	ids := map[string]bool{}
	page := ""
	for {
		var r struct {
			History []struct {
				Messages []Message `json:"messages"`
			} `json:"history"`
			Next      string `json:"nextPageToken"`
			HistoryID string `json:"historyId"`
		}
		err := c.call(ctx, "GET", "/history?startHistoryId="+url.QueryEscape(start)+"&maxResults=500&pageToken="+url.QueryEscape(page), nil, &r)
		if err != nil {
			return nil, "", err
		}
		for _, h := range r.History {
			for _, m := range h.Messages {
				ids[m.ID] = true
			}
		}
		page = r.Next
		if page == "" {
			result := make([]string, 0, len(ids))
			for id := range ids {
				result = append(result, id)
			}
			return result, r.HistoryID, nil
		}
	}
}
func (c *Client) Modify(ctx context.Context, id string, add, remove []string) (*Message, error) {
	var m Message
	err := c.call(ctx, "POST", "/messages/"+url.PathEscape(id)+"/modify", map[string]any{"addLabelIds": add, "removeLabelIds": remove}, &m)
	return &m, err
}
func (c *Client) Trash(ctx context.Context, id string) (*Message, error) {
	var m Message
	err := c.call(ctx, "POST", "/messages/"+url.PathEscape(id)+"/trash", struct{}{}, &m)
	return &m, err
}
func (c *Client) Untrash(ctx context.Context, id string) (*Message, error) {
	var m Message
	err := c.call(ctx, "POST", "/messages/"+url.PathEscape(id)+"/untrash", struct{}{}, &m)
	return &m, err
}
func (c *Client) Send(ctx context.Context, raw []byte) error {
	return c.call(ctx, "POST", "/messages/send", map[string]string{"raw": base64.RawURLEncoding.EncodeToString(raw)}, nil)
}
