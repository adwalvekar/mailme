package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

const MaxMessageBytes = 25 << 20

type smtpBackend struct{ b *Bridge }
type smtpSession struct {
	b             *Bridge
	authenticated bool
	from          string
	recipients    []string
}

func (b *Bridge) SMTPBackend() smtp.Backend { return &smtpBackend{b} }
func (b *smtpBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &smtpSession{b: b.b}, nil
}
func (s *smtpSession) AuthMechanisms() []string { return []string{sasl.Plain} }
func (s *smtpSession) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(identity, user, password string) error {
		if identity != "" && !strings.EqualFold(identity, user) {
			return smtp.ErrAuthFailed
		}
		if !s.b.Authenticate(user, password) {
			return smtp.ErrAuthFailed
		}
		s.authenticated = true
		return nil
	}), nil
}
func (s *smtpSession) Reset()              { s.from = ""; s.recipients = nil }
func (s *smtpSession) Logout() error       { return nil }
func smtpError(code int, msg string) error { return &smtp.SMTPError{Code: code, Message: msg} }
func (s *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	if !s.authenticated {
		return smtp.ErrAuthRequired
	}
	if !strings.EqualFold(from, s.b.email) {
		return smtpError(553, "Use the configured account as sender; aliases are not supported yet")
	}
	s.from = from
	return nil
}
func (s *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if !s.authenticated {
		return smtp.ErrAuthRequired
	}
	a, err := mail.ParseAddress(to)
	if err != nil || a.Address != to || strings.ContainsAny(to, "\r\n") {
		return smtpError(553, "Invalid recipient")
	}
	s.recipients = append(s.recipients, to)
	return nil
}
func (s *smtpSession) Data(r io.Reader) error {
	if !s.authenticated {
		return smtp.ErrAuthRequired
	}
	raw, err := io.ReadAll(io.LimitReader(r, MaxMessageBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > MaxMessageBytes {
		return smtpError(552, "Message exceeds 25 MiB limit")
	}
	raw, err = prepareSend(raw, s.from, s.recipients)
	if err != nil {
		return smtpError(554, err.Error())
	}
	ctx, cancel := context.WithTimeout(s.b.ctx, 60*time.Second)
	defer cancel()
	if err = s.b.api.Send(ctx, raw); err != nil {
		log.Printf("Gmail send failed (not retried by bridge): %v", err)
		// A transport failure can occur after Google accepts mail. A permanent
		// response prevents a client from silently retrying an ambiguous send.
		return smtpError(554, "Gmail did not confirm delivery. Check Gmail Sent before manually retrying")
	}
	return nil
}

// Gmail derives recipients from MIME headers; SMTP derives them from RCPT TO.
// Reject recipients present only in headers, and add envelope-only recipients
// as Bcc without reserializing MIME bodies or attachments.
func prepareSend(raw []byte, from string, recipients []string) ([]byte, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("Invalid RFC 5322 message")
	}
	if len(m.Header["From"]) != 1 {
		return nil, errors.New("Exactly one From header is required")
	}
	senders, err := m.Header.AddressList("From")
	if err != nil || len(senders) != 1 || !strings.EqualFold(senders[0].Address, from) {
		return nil, errors.New("From header must match authenticated sender")
	}
	for key := range m.Header {
		if strings.HasPrefix(strings.ToLower(key), "resent-") {
			return nil, errors.New("Resent headers are not supported")
		}
	}
	envelope := map[string]string{}
	for _, r := range recipients {
		envelope[strings.ToLower(r)] = r
	}
	if len(envelope) == 0 {
		return nil, errors.New("At least one recipient is required")
	}
	visible := map[string]bool{}
	for _, headerName := range []string{"To", "Cc", "Bcc"} {
		for _, value := range m.Header[headerName] {
			if strings.TrimSpace(value) == "" {
				continue
			}
			addresses, err := mail.ParseAddressList(value)
			if err != nil {
				return nil, fmt.Errorf("Invalid %s header", headerName)
			}
			for _, a := range addresses {
				key := strings.ToLower(a.Address)
				if _, ok := envelope[key]; !ok {
					return nil, errors.New("Header recipient is missing from SMTP envelope")
				}
				if headerName != "Bcc" {
					visible[key] = true
				}
			}
		}
	}
	var missing []string
	for _, r := range recipients {
		key := strings.ToLower(r)
		if !visible[key] {
			missing = append(missing, (&mail.Address{Address: r}).String())
			visible[key] = true
		}
	}
	if len(missing) == 0 && len(m.Header["Bcc"]) == 0 {
		return raw, nil
	}
	// Replace existing (possibly folded) Bcc fields with a single envelope-derived
	// field. Leave all other headers and the MIME body byte-for-byte intact.
	end, nl := bytes.Index(raw, []byte("\r\n\r\n")), 2
	if end < 0 {
		end, nl = bytes.Index(raw, []byte("\n\n")), 1
	}
	if end < 0 {
		return nil, errors.New("Missing header/body separator")
	}
	var out bytes.Buffer
	if len(missing) > 0 {
		out.WriteString("Bcc: " + strings.Join(missing, ",\r\n ") + "\r\n")
	}
	skip := false
	for _, line := range bytes.SplitAfter(raw[:end+nl], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			key, _, _ := bytes.Cut(line, []byte(":"))
			skip = strings.EqualFold(string(key), "Bcc")
		}
		if !skip {
			out.Write(line)
		}
	}
	out.Write(raw[end+nl:])
	return out.Bytes(), nil
}
