package bridge

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	stdsmtp "net/smtp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-smtp"
	"mailme/internal/gmail"
	"mailme/internal/localtls"
)

const testEmail = "me@example.com"
const testPassword = "test-only-password-with-32-characters"
const fixture = "From: sender@example.com\r\nTo: me@example.com\r\nSubject: Hello bridge\r\nDate: Sun, 18 Oct 2026 10:00:00 -0700\r\nMessage-ID: <hello@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=boundary\r\n\r\n--boundary\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nHello caf=C3=A9\r\n--boundary\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=example.bin\r\nContent-Transfer-Encoding: base64\r\n\r\nAAECAwQ=\r\n--boundary--\r\n"

type fakeAPI struct {
	mu                   sync.Mutex
	messages             map[string]gmail.Message
	history              string
	historyErr, errorGet error
	failID               string
	sent                 [][]byte
	sendErr              error
}

func newFake() *fakeAPI {
	return &fakeAPI{history: "10", messages: map[string]gmail.Message{"a": {ID: "a", LabelIDs: []string{"INBOX", "UNREAD"}, InternalDate: "1792342800000", Raw: base64.RawURLEncoding.EncodeToString([]byte(fixture))}}}
}
func (f *fakeAPI) Profile(context.Context) (*gmail.Profile, error) {
	return &gmail.Profile{Email: testEmail, HistoryID: f.history}, nil
}
func (f *fakeAPI) Labels(context.Context) ([]gmail.Label, error) {
	return []gmail.Label{{ID: "label1", Name: "Projects/Test", Type: "user"}}, nil
}
func (f *fakeAPI) IDs(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.messages {
		ids = append(ids, id)
	}
	return ids, nil
}
func (f *fakeAPI) Message(_ context.Context, id, format string) (*gmail.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == f.failID && f.errorGet != nil {
		return nil, f.errorGet
	}
	m, ok := f.messages[id]
	if !ok {
		return nil, &gmail.APIError{Status: 404}
	}
	m.LabelIDs = append([]string{}, m.LabelIDs...)
	if format != "raw" {
		m.Raw = ""
	}
	return &m, nil
}
func (f *fakeAPI) Changes(ctx context.Context, _ string) ([]string, string, error) {
	ids, err := f.IDs(ctx)
	if f.historyErr != nil {
		return nil, "", f.historyErr
	}
	return ids, f.history, err
}
func (f *fakeAPI) Modify(_ context.Context, id string, add, remove []string) (*gmail.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.messages[id]
	if !ok {
		return nil, &gmail.APIError{Status: 404}
	}
	var labels []string
	for _, l := range m.LabelIDs {
		if !contains(remove, l) {
			labels = append(labels, l)
		}
	}
	for _, l := range add {
		if !contains(labels, l) {
			labels = append(labels, l)
		}
	}
	m.LabelIDs = labels
	f.messages[id] = m
	m.Raw = ""
	return &m, nil
}
func (f *fakeAPI) Trash(ctx context.Context, id string) (*gmail.Message, error) {
	return f.Modify(ctx, id, []string{"TRASH"}, nil)
}
func (f *fakeAPI) Untrash(ctx context.Context, id string) (*gmail.Message, error) {
	return f.Modify(ctx, id, nil, []string{"TRASH"})
}
func (f *fakeAPI) Send(_ context.Context, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, append([]byte{}, raw...))
	return nil
}
func openTest(t *testing.T, f *fakeAPI) *Bridge {
	t.Helper()
	b, err := Open(t.TempDir(), testEmail, testPassword, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	if err = b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUIDPersistenceAndReentry(t *testing.T) {
	f := newFake()
	dir := t.TempDir()
	b, err := Open(dir, testEmail, testPassword, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := b.state.Folders["INBOX"].Members["a"]
	validity := b.state.Folders["INBOX"].Validity
	b.Close()
	b, err = Open(dir, testEmail, testPassword, f)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.state.Folders["INBOX"].Members["a"] != first || b.state.Folders["INBOX"].Validity != validity {
		t.Fatal("UID identity changed across restart")
	}
	f.Modify(context.Background(), "a", nil, []string{"INBOX"})
	if err = b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.state.Folders["INBOX"].Members) != 0 {
		t.Fatal("removed membership retained")
	}
	f.Modify(context.Background(), "a", []string{"INBOX"}, nil)
	if err = b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.state.Folders["INBOX"].Members["a"] <= first {
		t.Fatal("UID reused after re-entry")
	}
}
func TestSyncFailureIsAtomicAndExpiredHistoryFallsBack(t *testing.T) {
	f := newFake()
	b := openTest(t, f)
	f.messages["b"] = gmail.Message{ID: "b", LabelIDs: []string{"INBOX"}, InternalDate: "1792342800001"}
	f.history = "20"
	f.failID = "b"
	f.errorGet = errors.New("interrupted")
	if b.Sync(context.Background()) == nil {
		t.Fatal("expected failed sync")
	}
	if b.state.History != "10" || len(b.state.Messages) != 1 {
		t.Fatal("partial snapshot committed")
	}
	f.failID = ""
	f.historyErr = &gmail.APIError{Status: 404}
	delete(f.messages, "a")
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.state.History != "20" || b.state.Messages["a"] != nil || b.state.Messages["b"] == nil {
		t.Fatal("full-sync recovery failed")
	}
}
func TestDeletionSemanticsAndReadOnly(t *testing.T) {
	f := newFake()
	b := openTest(t, f)
	s := b.NewSession()
	defer s.Close()
	_, err := s.Select("INBOX", &imap.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	set := imap.UIDSetNum(b.state.Folders["INBOX"].Members["a"])
	if err = s.Store(nil, set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}, Silent: true}, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Expunge(nil, nil); err != nil {
		t.Fatal(err)
	}
	if contains(f.messages["a"].LabelIDs, "INBOX") || contains(f.messages["a"].LabelIDs, "TRASH") {
		t.Fatal("Inbox deletion should archive")
	}
	s.Select("Gmail/All Mail", &imap.SelectOptions{ReadOnly: true})
	if s.Store(nil, imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}, Silent: true}, nil) == nil {
		t.Fatal("EXAMINE allowed STORE")
	}
	s.Select("Gmail/All Mail", &imap.SelectOptions{})
	s.Store(nil, imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}, Silent: true}, nil)
	if err = s.Expunge(nil, nil); err != nil {
		t.Fatal(err)
	}
	if !contains(f.messages["a"].LabelIDs, "TRASH") {
		t.Fatal("All Mail deletion should trash")
	}
	s.Select("Gmail/Trash", &imap.SelectOptions{})
	s.Store(nil, imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}, Silent: true}, nil)
	if s.Expunge(nil, nil) == nil {
		t.Fatal("permanent deletion should fail")
	}
}
func TestPrepareSend(t *testing.T) {
	base := "From: Me <me@example.com>\r\nTo: to@example.com\r\nSubject: attachments unchanged\r\n\r\n\x00binary\xff\r\n"
	raw, err := prepareSend([]byte(base), testEmail, []string{"to@example.com", "hidden@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Header.Get("Bcc"), "hidden@example.com") {
		t.Fatal("envelope-only Bcc lost")
	}
	body, _ := io.ReadAll(m.Body)
	if string(body) != "\x00binary\xff\r\n" {
		t.Fatal("MIME bytes changed")
	}
	if _, err = prepareSend([]byte(base), testEmail, []string{"different@example.com"}); err == nil {
		t.Fatal("unexpected header recipient accepted")
	}
	if _, err = prepareSend([]byte(strings.Replace(base, "me@example.com", "attacker@example.com", 1)), testEmail, []string{"to@example.com"}); err == nil {
		t.Fatal("spoofed From accepted")
	}
	bccOnly := "From: me@example.com\r\nTo: undisclosed-recipients:;\r\n\r\nHello\r\n"
	if _, err = prepareSend([]byte(bccOnly), testEmail, []string{"hidden@example.com"}); err != nil {
		t.Fatalf("Bcc-only message: %v", err)
	}
}

type wire struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	n    int
}

func (w *wire) command(cmd string) string {
	w.t.Helper()
	w.conn.SetDeadline(time.Now().Add(10 * time.Second))
	w.n++
	tag := fmt.Sprintf("A%d", w.n)
	if _, err := fmt.Fprintf(w.conn, "%s %s\r\n", tag, cmd); err != nil {
		w.t.Fatal(err)
	}
	var result strings.Builder
	for {
		line, err := w.r.ReadString('\n')
		if err != nil {
			w.t.Fatal(err)
		}
		result.WriteString(line)
		if strings.HasPrefix(line, tag+" ") {
			return result.String()
		}
	}
}
func (w *wire) ok(cmd string) string {
	w.t.Helper()
	out := w.command(cmd)
	if !strings.Contains(out, fmt.Sprintf("A%d OK", w.n)) {
		w.t.Fatalf("%s failed: %s", cmd, out)
	}
	return out
}
func startServers(t *testing.T, b *Bridge) (string, string, *tls.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := localtls.Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := os.ReadFile(filepath.Join(dir, "localhost.crt"))
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	clientTLS := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "localhost"}
	il, err := localtls.Listen("127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	sl, err := localtls.Listen("127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	im := imapserver.New(&imapserver.Options{NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return b.NewSession(), nil, nil
	}, Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSpecialUse: {}}})
	sm := smtp.NewServer(b.SMTPBackend())
	sm.Domain = "localhost"
	sm.MaxMessageBytes = MaxMessageBytes
	go im.Serve(il)
	go sm.Serve(sl)
	t.Cleanup(func() { im.Close(); sm.Close(); il.Close(); sl.Close() })
	return il.Addr().String(), sl.Addr().String(), clientTLS
}
func dialWire(t *testing.T, addr string, cfg *tls.Config) *wire {
	t.Helper()
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	w := &wire{t: t, conn: c, r: bufio.NewReader(c)}
	line, err := w.r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "* OK") {
		t.Fatalf("greeting: %s %v", line, err)
	}
	return w
}

func TestMultipleIMAPSessionsAndIdle(t *testing.T) {
	f := newFake()
	for i, id := range []string{"b", "c"} {
		m := f.messages["a"]
		m.ID = id
		m.InternalDate = fmt.Sprint(1792342800001 + i)
		f.messages[id] = m
	}
	b := openTest(t, f)
	ia, _, cfg := startServers(t, b)
	first := dialWire(t, ia, cfg)
	second := dialWire(t, ia, cfg)
	for _, w := range []*wire{first, second} {
		w.ok("LOGIN me@example.com " + testPassword)
		w.ok("SELECT INBOX")
	}
	first.ok("UID STORE 1 +FLAGS.SILENT (\\Deleted)")
	first.ok("EXPUNGE")
	if out := second.ok("FETCH 1 (UID)"); strings.Contains(out, "UID 2") || strings.Contains(out, "UID 3") {
		t.Fatalf("stale sequence number targeted another message: %s", out)
	}
	if out := second.ok("FETCH 2 (UID)"); !strings.Contains(out, "2 FETCH (UID 2") {
		t.Fatal(out)
	}
	if out := second.ok("NOOP"); !strings.Contains(out, "1 EXPUNGE") {
		t.Fatal(out)
	}
	if out := second.ok("FETCH 1 (UID)"); !strings.Contains(out, "1 FETCH (UID 2") {
		t.Fatal(out)
	}
	// A burst of arrivals must be visible to both existing sessions.
	f.mu.Lock()
	for i, id := range []string{"d", "e"} {
		m := f.messages["b"]
		m.ID = id
		m.InternalDate = fmt.Sprint(1792342800004 + i)
		f.messages[id] = m
	}
	f.mu.Unlock()
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	second.ok("NOOP")
	if out := second.ok("UID FETCH 1:* (UID)"); strings.Count(out, " FETCH (") != 4 {
		t.Fatal(out)
	}
	// IDLE receives updates without issuing NOOP or another IMAP command.
	first.ok("NOOP")
	fmt.Fprint(first.conn, "ID IDLE\r\n")
	if line, err := first.r.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatalf("IDLE: %s %v", line, err)
	}
	f.mu.Lock()
	m := f.messages["b"]
	m.ID = "f"
	m.InternalDate = "1792342800009"
	f.messages["f"] = m
	f.mu.Unlock()
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sawExists bool
	for !sawExists {
		line, err := first.r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		sawExists = strings.Contains(line, "5 EXISTS")
	}
	fmt.Fprint(first.conn, "DONE\r\n")
	for {
		line, err := first.r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "ID OK") {
			break
		}
	}
	first.ok("LOGOUT")
	second.ok("LOGOUT")
}

func TestBccMergeAndSendFailure(t *testing.T) {
	raw := []byte("From: me@example.com\r\nTo: to@example.com\r\nBcc: old@example.com,\r\n another@example.com\r\nSubject: Test\r\n\r\nExact body\r\n")
	out, err := prepareSend(raw, testEmail, []string{"to@example.com", "old@example.com", "another@example.com", "new@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(strings.NewReader(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	bcc, err := m.Header.AddressList("Bcc")
	if err != nil || len(bcc) != 3 || len(m.Header["Bcc"]) != 1 {
		t.Fatalf("BCC merge: %v %v", bcc, err)
	}
	body, _ := io.ReadAll(m.Body)
	if string(body) != "Exact body\r\n" {
		t.Fatal("body changed")
	}
	f := newFake()
	f.sendErr = errors.New("connection lost after submission")
	b := openTest(t, f)
	s := &smtpSession{b: b, authenticated: true, from: testEmail, recipients: []string{"to@example.com"}}
	err = s.Data(strings.NewReader("From: me@example.com\r\nTo: to@example.com\r\n\r\nHello\r\n"))
	var smtpErr *smtp.SMTPError
	if !errors.As(err, &smtpErr) || smtpErr.Code != 554 {
		t.Fatalf("ambiguous submission should not trigger automatic retries: %v", err)
	}
}
func TestIMAPAndSMTPOverTLS(t *testing.T) {
	f := newFake()
	b := openTest(t, f)
	ia, sa, cfg := startServers(t, b)
	w := dialWire(t, ia, cfg)
	if out := w.command("LOGIN me@example.com wrong"); !strings.Contains(out, " NO ") {
		t.Fatal("bad credentials accepted")
	}
	w.ok("LOGIN me@example.com " + testPassword)
	if out := w.ok("LIST \"\" \"\""); !strings.Contains(out, "\\Noselect") {
		t.Fatal(out)
	}
	if out := w.ok("LIST \"\" \"%\""); !strings.Contains(out, "Gmail") || !strings.Contains(out, "Labels") {
		t.Fatal(out)
	}
	if out := w.ok("LIST \"\" \"*\""); !strings.Contains(out, "Gmail/Sent") || !strings.Contains(out, "Projects/Test") {
		t.Fatal(out)
	}
	w.ok("SELECT INBOX")
	if out := w.ok("UID FETCH 1 (UID FLAGS INTERNALDATE RFC822.SIZE ENVELOPE BODYSTRUCTURE BODY.PEEK[1])"); !strings.Contains(out, "Hello caf=C3=A9") || !strings.Contains(out, "example.bin") {
		t.Fatal(out)
	}
	if !contains(f.messages["a"].LabelIDs, "UNREAD") {
		t.Fatal("PEEK marked message seen")
	}
	w.ok("UID FETCH 1 (BODY[TEXT])")
	if contains(f.messages["a"].LabelIDs, "UNREAD") {
		t.Fatal("BODY fetch did not mark seen")
	}
	if out := w.ok("UID FETCH 1 (BODY.PEEK[1]<0.5>)"); !strings.Contains(out, "Hello") {
		t.Fatal(out)
	}
	w.ok("EXAMINE INBOX")
	if out := w.command("UID STORE 1 +FLAGS (\\Deleted)"); !strings.Contains(out, " NO ") {
		t.Fatal("EXAMINE permitted mutation")
	}
	w.ok("SELECT INBOX")
	w.ok("UID STORE 1 +FLAGS (\\Flagged)")
	if !contains(f.messages["a"].LabelIDs, "STARRED") {
		t.Fatal("flag did not reach Gmail")
	}
	if out := w.ok("UID SEARCH BODY \"café\""); !strings.Contains(out, "* SEARCH 1") {
		t.Fatal(out)
	}
	w.ok("UID COPY 1 \"Labels/Projects/Test\"")
	w.ok("UID STORE 1 +FLAGS.SILENT (\\Deleted)")
	if out := w.ok("EXPUNGE"); !strings.Contains(out, "1 EXPUNGE") {
		t.Fatal(out)
	}
	w.ok("LOGOUT")
	c, err := tls.Dial("tcp", sa, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := stdsmtp.NewClient(c, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	if sc.Mail(testEmail) == nil {
		t.Fatal("SMTP relay allowed without authentication")
	}
	if err = sc.Auth(stdsmtp.PlainAuth("", testEmail, testPassword, "localhost")); err != nil {
		t.Fatal(err)
	}
	if err = sc.Mail(testEmail); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"to@example.com", "hidden@example.com"} {
		if err = sc.Rcpt(r); err != nil {
			t.Fatal(err)
		}
	}
	d, err := sc.Data()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(d, "From: me@example.com\r\nTo: to@example.com\r\nSubject: Test\r\n\r\nHello\r\n")
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 || !strings.Contains(string(f.sent[0]), "Bcc: <hidden@example.com>") {
		t.Fatal("Gmail send did not preserve Bcc")
	}
}

func TestMalformedMessageDoesNotWedgeIMAP(t *testing.T) {
	f := newFake()
	m := f.messages["a"]
	m.Raw = base64.RawURLEncoding.EncodeToString([]byte("private malformed header\r\n\r\ncontent"))
	f.messages["a"] = m
	b := openTest(t, f)
	ia, _, cfg := startServers(t, b)
	w := dialWire(t, ia, cfg)
	w.ok("LOGIN me@example.com " + testPassword)
	w.ok("SELECT INBOX")
	if out := w.command("FETCH 1 (ENVELOPE)"); !strings.Contains(out, " NO ") {
		t.Fatal(out)
	}
	w.ok("NOOP")
	w.ok("LOGOUT")
}
