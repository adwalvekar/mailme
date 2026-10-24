package gmail

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError deliberately excludes Google's free-form message and response body.
// Status is the HTTP code; State and Reasons are bounded machine identifiers.
type APIError struct {
	Status     int
	State      string
	Reasons    []string
	Method     string
	ID         string
	Backend    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("Gmail API returned HTTP %d", e.Status)
	if e.Method != "" {
		s += " method=" + SafeIdentifier(e.Method)
	}
	if e.ID != "" {
		s += " id=" + SafeIdentifier(e.ID)
	}
	if e.State != "" {
		s += " status=" + SafeIdentifier(e.State)
	}
	if len(e.Reasons) > 0 {
		var reasons []string
		for _, r := range e.Reasons {
			reasons = append(reasons, SafeIdentifier(r))
		}
		s += " reasons=" + strings.Join(reasons, ",")
	}
	if e.Backend == "gws" && e.Status == 401 {
		s += "; gws credentials expired or lack Gmail access; run `gws auth login` and grant Gmail modify"
	}
	return s
}

// These fields must not become a way to print arbitrary provider text.
func SafeIdentifier(s string) string {
	if len(s) == 0 || len(s) > 128 {
		return "unknown"
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return "unknown"
		}
	}
	return s
}

// ParseAPIError accepts both Google's error envelope and gws's flattened reason.
// gws 0.22.5 drops status, extra reasons, and HTTP headers; never infer them.
func ParseAPIError(data []byte, status int) *APIError {
	e := &APIError{Status: status}
	var wire struct {
		Error struct {
			Code   int    `json:"code"`
			State  string `json:"status"`
			Reason string `json:"reason"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			RetryAfter json.RawMessage `json:"retryAfter"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &wire) != nil {
		return e
	}
	if wire.Error.Code >= 400 && wire.Error.Code <= 599 {
		e.Status = wire.Error.Code
	}
	if wire.Error.State != "" {
		e.State = SafeIdentifier(wire.Error.State)
	}
	add := func(s string) {
		if s == "" || len(e.Reasons) >= 8 {
			return
		}
		s = SafeIdentifier(s)
		for _, r := range e.Reasons {
			if r == s {
				return
			}
		}
		e.Reasons = append(e.Reasons, s)
	}
	add(wire.Error.Reason)
	for _, r := range wire.Error.Errors {
		add(r.Reason)
	}
	var retry string
	if json.Unmarshal(wire.Error.RetryAfter, &retry) != nil {
		retry = string(wire.Error.RetryAfter)
	}
	e.RetryAfter = RetryDelay(retry, time.Now())
	return e
}

func RetryDelay(value string, now time.Time) time.Duration {
	if n, err := strconv.Atoi(value); err == nil && n > 0 {
		if n > 60 {
			n = 60
		}
		return time.Duration(n) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		d := when.Sub(now)
		if d > time.Minute {
			d = time.Minute
		}
		if d > 0 {
			return d
		}
	}
	return 0
}

func Retryable(err error) bool {
	var e *APIError
	if !errors.As(err, &e) {
		return false
	}
	if e.Status == 429 || e.Status >= 500 {
		return true
	}
	if e.Status == 403 {
		for _, r := range e.Reasons {
			if r == "rateLimitExceeded" || r == "userRateLimitExceeded" {
				return true
			}
		}
	}
	return false
}

type MalformedMessageError struct{ ID string }

func (e *MalformedMessageError) Error() string {
	return "malformed Gmail message response method=messages.get id=" + SafeIdentifier(e.ID)
}

func PerMessage(err error) bool {
	var malformed *MalformedMessageError
	if errors.As(err, &malformed) {
		return true
	}
	var e *APIError
	if !errors.As(err, &e) {
		return false
	}
	if e.Status == 404 {
		return true
	}
	if e.Status != 400 {
		return false
	}
	if e.State == "INVALID_ARGUMENT" || e.State == "FAILED_PRECONDITION" {
		return true
	}
	for _, r := range e.Reasons {
		if r == "invalidArgument" || r == "failedPrecondition" {
			return true
		}
	}
	return false
}

func ValidateMessage(m *Message, id, format string) error {
	if m == nil || m.ID == "" || m.ID != id {
		return &MalformedMessageError{ID: id}
	}
	if format == "metadata" {
		if _, err := strconv.ParseInt(m.InternalDate, 10, 64); err != nil {
			return &MalformedMessageError{ID: id}
		}
	}
	if format == "raw" && m.Raw == "" {
		return &MalformedMessageError{ID: id}
	}
	return nil
}
