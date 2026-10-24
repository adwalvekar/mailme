package bridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	message "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"mailme/internal/gmail"
)

func (s *Session) Search(kind imapserver.NumKind, c *imap.SearchCriteria, _ *imap.SearchOptions) (*imap.SearchData, error) {
	s.b.mu.Lock()
	ctx, cancel := context.WithTimeout(s.b.ctx, 15*time.Minute)
	defer cancel()
	messages := s.selected(imap.SeqSet{{Start: 1, Stop: 0}})
	// Keep immutable records so flags/date filters see one command snapshot.
	records := make(map[string]*Record, len(messages))
	for _, m := range messages {
		records[m.id] = s.b.state.Messages[m.id]
	}
	s.b.mu.Unlock()
	var seqs imap.SeqSet
	var uids imap.UIDSet
	var maxSeq uint32
	var maxUID imap.UID
	for _, m := range messages {
		if m.seq > maxSeq {
			maxSeq = m.seq
		}
		if m.uid > maxUID {
			maxUID = m.uid
		}
	}
	data := &imap.SearchData{}
	for _, m := range messages {
		r := records[m.id]
		var raw []byte
		getRaw := func() ([]byte, error) {
			if raw != nil {
				return raw, nil
			}
			var err error
			raw, err = s.b.raw(ctx, m.id)
			return raw, err
		}
		ok, err := matchSearch(c, r, flags(r, s.folder), m, maxSeq, maxUID, getRaw)
		if err != nil {
			return nil, protocolError(err)
		}
		if !ok {
			continue
		}
		seqs.AddNum(m.seq)
		uids.AddNum(m.uid)
		n := m.seq
		if kind == imapserver.NumKindUID {
			n = uint32(m.uid)
		}
		if data.Min == 0 || n < data.Min {
			data.Min = n
		}
		if n > data.Max {
			data.Max = n
		}
		data.Count++
	}
	if kind == imapserver.NumKindUID {
		data.All = uids
	} else {
		data.All = seqs
	}
	return data, nil
}
func matchSearch(c *imap.SearchCriteria, r *Record, fs []imap.Flag, m selected, maxSeq uint32, maxUID imap.UID, raw func() ([]byte, error)) (bool, error) {
	for _, set := range c.SeqNum {
		ok := false
		for _, v := range set {
			ok = ok || rangeContains(v.Start, v.Stop, m.seq, maxSeq)
		}
		if !ok {
			return false, nil
		}
	}
	for _, set := range c.UID {
		ok := false
		for _, v := range set {
			ok = ok || rangeContains(uint32(v.Start), uint32(v.Stop), uint32(m.uid), uint32(maxUID))
		}
		if !ok {
			return false, nil
		}
	}
	for _, f := range c.Flag {
		if !hasFlag(fs, f) {
			return false, nil
		}
	}
	for _, f := range c.NotFlag {
		if hasFlag(fs, f) {
			return false, nil
		}
	}
	ms, _ := strconv.ParseInt(r.Message.InternalDate, 10, 64)
	if !dateMatches(time.UnixMilli(ms), c.Since, c.Before) {
		return false, nil
	}
	if len(c.Header)+len(c.Body)+len(c.Text) > 0 || c.Larger > 0 || c.Smaller > 0 || !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		e := metadataEntity(c, r)
		if e == nil {
			buf, err := raw()
			if err != nil {
				return false, err
			}
			if c.Larger > 0 && int64(len(buf)) <= c.Larger || c.Smaller > 0 && int64(len(buf)) >= c.Smaller {
				return false, nil
			}
			e, _ = message.Read(bytes.NewReader(buf))
			if e == nil {
				return false, errors.New("cannot parse message MIME content")
			}
		}
		for _, h := range c.Header {
			ok := false
			fields := e.Header.FieldsByKey(h.Key)
			for fields.Next() {
				value, _ := fields.Text()
				if textContains(value, h.Value) {
					ok = true
				}
			}
			if !ok {
				return false, nil
			}
		}
		if !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
			d, err := mail.ParseDate(e.Header.Get("Date"))
			if err != nil || !dateMatches(d, c.SentSince, c.SentBefore) {
				return false, nil
			}
		}
		if len(c.Body)+len(c.Text) > 0 {
			body, err := decodedText(e)
			if err != nil {
				return false, errors.New("cannot decode message MIME content")
			}
			var headers strings.Builder
			fields := e.Header.Fields()
			for fields.Next() {
				value, _ := fields.Text()
				headers.WriteString(fields.Key() + ": " + value + "\n")
			}
			for _, q := range c.Body {
				if !textContains(body, q) {
					return false, nil
				}
			}
			for _, q := range c.Text {
				if !textContains(headers.String()+body, q) {
					return false, nil
				}
			}
		}
	}
	for i := range c.Not {
		ok, err := matchSearch(&c.Not[i], r, fs, m, maxSeq, maxUID, raw)
		if err != nil {
			return false, err
		}
		if ok {
			return false, nil
		}
	}
	for i := range c.Or {
		a, err := matchSearch(&c.Or[i][0], r, fs, m, maxSeq, maxUID, raw)
		if err != nil {
			return false, err
		}
		if a {
			continue
		}
		b, err := matchSearch(&c.Or[i][1], r, fs, m, maxSeq, maxUID, raw)
		if err != nil || !b {
			return false, err
		}
	}
	return true, nil
}

func metadataEntity(c *imap.SearchCriteria, r *Record) *message.Entity {
	if r.Message.Payload == nil || len(c.Body)+len(c.Text) > 0 || c.Larger > 0 || c.Smaller > 0 {
		return nil
	}
	for _, header := range c.Header {
		included := false
		for _, name := range gmail.MetadataHeaders {
			if strings.EqualFold(header.Key, name) {
				included = true
				break
			}
		}
		// IMAP HEADER accepts arbitrary names; absence from our projection does
		// not mean absence from the real message. Fall back to the complete MIME.
		if !included {
			return nil
		}
	}
	e := &message.Entity{}
	h := r.Message.Payload.Headers
	for i := len(h) - 1; i >= 0; i-- {
		e.Header.Add(h[i].Name, h[i].Value)
	}
	return e
}
func dateMatches(t, since, before time.Time) bool {
	d := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
	t = d(t)
	return (since.IsZero() || !t.Before(d(since))) && (before.IsZero() || t.Before(d(before)))
}
func textContains(value, query string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(query))
}
func decodedText(e *message.Entity) (string, error) {
	if parts := e.MultipartReader(); parts != nil {
		var b strings.Builder
		for {
			part, err := parts.NextPart()
			if err == io.EOF {
				break
			}
			if part == nil {
				return "", err
			}
			text, err := decodedText(part)
			if err != nil {
				return "", err
			}
			b.WriteString(text)
			b.WriteByte('\n')
		}
		return b.String(), nil
	}
	t, _, _ := e.Header.ContentType()
	if !strings.HasPrefix(t, "text/") {
		return "", nil
	}
	b, err := io.ReadAll(e.Body)
	return string(b), err
}
