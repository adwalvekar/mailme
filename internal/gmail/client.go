// Package gmail is the bridge's only mail transport. It uses HTTPS, never IMAP or SMTP.
package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const Endpoint = "https://gmail.googleapis.com/gmail/v1/users/me"

type Message struct {
	ID           string   `json:"id"`
	LabelIDs     []string `json:"labelIds"`
	InternalDate string   `json:"internalDate"`
	Raw          string   `json:"raw,omitempty"`
	ThreadID     string   `json:"threadId,omitempty"`
	Payload      *Payload `json:"payload,omitempty"`
}
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type Payload struct {
	Headers []Header `json:"headers"`
}

var MetadataHeaders = []string{"From", "To", "Cc", "Bcc", "Reply-To", "Sender", "Subject", "Date", "Message-ID", "In-Reply-To", "References"}

const MetadataFields = "id,labelIds,internalDate,threadId,payload/headers"

type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}
type Profile struct {
	Email     string `json:"emailAddress"`
	HistoryID string `json:"historyId"`
}
type Client struct {
	HTTP         *http.Client
	BaseURL      string // Override only in tests.
	Email        string
	QuotaProfile string // empty means current; set before making any requests
	writeMu      sync.Mutex
	pace         Pacer
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	base := c.BaseURL
	if base == "" {
		base = Endpoint
		if c.Email != "" {
			base = "https://gmail.googleapis.com/gmail/v1/users/" + url.PathEscape(c.Email)
		}
	}
	if method != http.MethodGet {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
	}
	apiMethod, id := requestIdentity(path)
	_, query, _ := strings.Cut(path, "?")
	values, _ := url.ParseQuery(query)
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
		if err := c.pace.Wait(ctx, Cost(apiMethod, values.Get("format"), c.QuotaProfile), c.QuotaProfile); err != nil {
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
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("Gmail API transport failed method=" + apiMethod + " id=" + SafeIdentifier(id))
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			if out != nil {
				err = json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(out)
			}
			res.Body.Close()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if apiMethod == "messages.get" {
					return &MalformedMessageError{ID: id}
				}
				return errors.New("invalid Gmail API response method=" + apiMethod)
			}
			return err
		}
		data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		apiErr := ParseAPIError(data, res.StatusCode)
		apiErr.Method, apiErr.ID = apiMethod, id
		apiErr.RetryAfter = RetryDelay(res.Header.Get("Retry-After"), time.Now())
		res.Body.Close()
		if method != http.MethodGet || attempt >= 3 || !Retryable(apiErr) {
			return apiErr
		}
		delay := time.Second << attempt
		if apiErr.RetryAfter > delay {
			delay = apiErr.RetryAfter
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
}
func wait(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func requestIdentity(path string) (string, string) {
	path, _, _ = strings.Cut(path, "?")
	p := strings.Split(strings.Trim(path, "/"), "/")
	if p[0] == "profile" {
		return "getProfile", ""
	}
	if len(p) == 1 {
		return p[0] + ".list", ""
	}
	if p[1] == "send" {
		return "messages.send", ""
	}
	id, _ := url.PathUnescape(p[1])
	if len(p) == 3 {
		return "messages." + p[2], id
	}
	return "messages.get", id
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
	return c.listIDs(ctx, "")
}
func (c *Client) InboxIDs(ctx context.Context) ([]string, error) { return c.listIDs(ctx, "INBOX") }
func (c *Client) listIDs(ctx context.Context, label string) ([]string, error) {
	var ids []string
	page := ""
	for {
		var r struct {
			Messages []Message `json:"messages"`
			Next     string    `json:"nextPageToken"`
		}
		q := url.Values{"includeSpamTrash": {"true"}, "maxResults": {"500"}, "fields": {"messages/id,nextPageToken"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		if label != "" {
			q.Set("labelIds", label)
		}
		err := c.call(ctx, "GET", "/messages?"+q.Encode(), nil, &r)
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
	q := url.Values{"format": {format}}
	if format == "metadata" {
		q.Set("fields", MetadataFields)
		q["metadataHeaders"] = append([]string(nil), MetadataHeaders...)
	} else if format == "raw" {
		q.Set("fields", "id,raw")
	}
	err := c.call(ctx, "GET", "/messages/"+url.PathEscape(id)+"?"+q.Encode(), nil, &m)
	if err == nil {
		err = ValidateMessage(&m, id, format)
	}
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
	var m Message
	if err := c.call(ctx, "POST", "/messages/send", map[string]string{"raw": base64.RawURLEncoding.EncodeToString(raw)}, &m); err != nil {
		return err
	}
	if m.ID == "" {
		return errors.New("Gmail send returned no message ID; check Gmail Sent before retrying")
	}
	return nil
}
