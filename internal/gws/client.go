// Package gws accesses Gmail through an installed Google Workspace CLI. The CLI
// owns Google credentials and token refresh; Mailme never exports its tokens.
package gws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mailme/internal/gmail"
)

type Client struct {
	path, email string
	gate        chan struct{}
	next        time.Time // protected by gate, including while a command is running
}

func New(path, email string) (*Client, error) {
	if email == "" {
		return nil, errors.New("gws backend requires an account email")
	}
	if path == "" {
		path = "gws"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return nil, errors.New("cannot find gws; use login --backend gws --gws-path /absolute/path/to/gws")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	return &Client{path: resolved, email: email, gate: make(chan struct{}, 1)}, nil
}

func (c *Client) Path() string { return c.path }

func (c *Client) call(ctx context.Context, resource, method string, params map[string]any, body any, upload string, read bool, out any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// Serializing subprocesses avoids overlapping CLI credential refreshes.
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if params == nil {
		params = make(map[string]any)
	}
	// Never use "me": an accidental change of the CLI's active account must not
	// silently read another mailbox or submit mail as a different sender.
	params["userId"] = c.email
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	args := []string{"gmail", "users"}
	if resource != "" {
		args = append(args, resource)
	}
	args = append(args, method, "--params", string(p), "--format", "json")
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		args = append(args, "--json", string(b))
	}
	if upload != "" {
		args = append(args, "--upload", upload, "--upload-content-type", "message/rfc822")
	}
	for attempt := 0; ; attempt++ {
		if err := wait(ctx, time.Until(c.next)); err != nil {
			return err
		}
		c.next = time.Now().Add(40 * time.Millisecond)
		err := c.invoke(ctx, args, out)
		var apiErr *gmail.APIError
		if !read || attempt >= 3 || !errors.As(err, &apiErr) || (apiErr.Status != 429 && apiErr.Status < 500) {
			return err
		}
		if err := wait(ctx, time.Second<<attempt); err != nil {
			return err
		}
	}
}

func wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) Profile(ctx context.Context) (*gmail.Profile, error) {
	var p gmail.Profile
	if err := c.call(ctx, "", "getProfile", nil, nil, "", true, &p); err != nil {
		return nil, err
	}
	if !strings.EqualFold(p.Email, c.email) {
		return nil, errors.New("gws account does not match configured email; check the CLI's active credentials")
	}
	if p.HistoryID == "" {
		return nil, errors.New("gws returned a profile without a history checkpoint")
	}
	return &p, nil
}

func (c *Client) Labels(ctx context.Context) ([]gmail.Label, error) {
	var r struct {
		Labels []gmail.Label `json:"labels"`
	}
	err := c.call(ctx, "labels", "list", nil, nil, "", true, &r)
	if err == nil && r.Labels == nil {
		err = errors.New("gws returned no labels collection")
	}
	return r.Labels, err
}

func (c *Client) IDs(ctx context.Context) ([]string, error) {
	var ids []string
	params := map[string]any{"includeSpamTrash": true, "maxResults": 500}
	seen := make(map[string]bool)
	for {
		var r struct {
			Messages []gmail.Message `json:"messages"`
			Next     string          `json:"nextPageToken"`
		}
		if err := c.call(ctx, "messages", "list", params, nil, "", true, &r); err != nil {
			return nil, err
		}
		for _, m := range r.Messages {
			if m.ID == "" {
				return nil, errors.New("gws returned a message without an ID")
			}
			ids = append(ids, m.ID)
		}
		if r.Next == "" {
			return ids, nil
		}
		if seen[r.Next] {
			return nil, errors.New("gws returned a repeated pagination token")
		}
		seen[r.Next] = true
		params["pageToken"] = r.Next
	}
}

func (c *Client) Message(ctx context.Context, id, format string) (*gmail.Message, error) {
	m, err := c.message(ctx, "get", map[string]any{"id": id, "format": format}, nil, true)
	if err == nil && format == "raw" && m.Raw == "" {
		return nil, errors.New("gws returned no raw MIME body")
	}
	return m, err
}

func (c *Client) Changes(ctx context.Context, start string) ([]string, string, error) {
	ids := make(map[string]bool)
	seen := make(map[string]bool)
	params := map[string]any{"startHistoryId": start, "maxResults": 500}
	for {
		var r struct {
			History []struct {
				Messages []gmail.Message `json:"messages"`
			} `json:"history"`
			Next      string `json:"nextPageToken"`
			HistoryID string `json:"historyId"`
		}
		if err := c.call(ctx, "history", "list", params, nil, "", true, &r); err != nil {
			return nil, "", err
		}
		for _, h := range r.History {
			for _, m := range h.Messages {
				if m.ID == "" {
					return nil, "", errors.New("gws returned history without a message ID")
				}
				ids[m.ID] = true
			}
		}
		if r.Next == "" {
			if r.HistoryID == "" {
				return nil, "", errors.New("gws returned history without a checkpoint")
			}
			result := make([]string, 0, len(ids))
			for id := range ids {
				result = append(result, id)
			}
			return result, r.HistoryID, nil
		}
		if seen[r.Next] {
			return nil, "", errors.New("gws returned a repeated pagination token")
		}
		seen[r.Next] = true
		params["pageToken"] = r.Next
	}
}

func (c *Client) message(ctx context.Context, method string, params map[string]any, body any, read bool) (*gmail.Message, error) {
	var m gmail.Message
	if err := c.call(ctx, "messages", method, params, body, "", read, &m); err != nil {
		return nil, err
	}
	if m.ID == "" || m.ID != params["id"] {
		return nil, fmt.Errorf("gws %s returned an unexpected message ID", method)
	}
	return &m, nil
}

func (c *Client) Modify(ctx context.Context, id string, add, remove []string) (*gmail.Message, error) {
	// JSON null is not an array in the CLI's Discovery schema validation.
	if add == nil {
		add = []string{}
	}
	if remove == nil {
		remove = []string{}
	}
	return c.message(ctx, "modify", map[string]any{"id": id}, map[string]any{"addLabelIds": add, "removeLabelIds": remove}, false)
}

func (c *Client) Trash(ctx context.Context, id string) (*gmail.Message, error) {
	return c.message(ctx, "trash", map[string]any{"id": id}, nil, false)
}

func (c *Client) Untrash(ctx context.Context, id string) (*gmail.Message, error) {
	return c.message(ctx, "untrash", map[string]any{"id": id}, nil, false)
}

func (c *Client) Send(ctx context.Context, raw []byte) error {
	// --json takes a literal, not stdin or @file. Upload avoids OS argument size
	// limits and exposing MIME bodies in the process list. Both directory and
	// file are private and are removed even if the command fails or times out.
	dir, err := os.MkdirTemp("", "mailme-send-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "message.eml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return err
	}
	var m gmail.Message
	if err := c.call(ctx, "messages", "send", nil, nil, path, false, &m); err != nil {
		return err
	}
	if m.ID == "" {
		return errors.New("gws send returned no message ID; check Gmail Sent before retrying")
	}
	return nil
}
