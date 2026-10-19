package bridge

import (
	"bufio"
	"bytes"
	"context"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"
)

var supportedFlags = []imap.Flag{imap.FlagSeen, imap.FlagFlagged, imap.FlagAnswered, imap.FlagDeleted}

type Session struct {
	viewMu   sync.Mutex
	b        *Bridge
	folder   string
	tracker  *imapserver.SessionTracker
	readOnly bool
}

var _ imapserver.Session = (*Session)(nil)

func (b *Bridge) NewSession() *Session { return &Session{b: b} }
func no(text string) error             { return &imap.Error{Type: imap.StatusResponseTypeNo, Text: text} }
func protocolError(err error) error {
	if err == nil {
		return nil
	}
	log.Printf("Mail operation failed: %v", err)
	return no("Gmail/cache operation failed; retry after checking bridge logs")
}
func (s *Session) Login(user, password string) error {
	if !s.b.Authenticate(user, password) {
		return imapserver.ErrAuthFailed
	}
	return nil
}
func (s *Session) Close() error { return s.Unselect() }
func (s *Session) Unselect() error {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	if s.tracker != nil {
		s.tracker.Close()
		s.tracker = nil
	}
	s.folder = ""
	return nil
}
func (s *Session) find(name string) (*Folder, error) {
	for _, f := range s.b.state.Folders {
		if f.Active && (f.Name == name || f.ID == "INBOX" && strings.EqualFold(name, "INBOX")) {
			return f, nil
		}
	}
	return nil, no("No such mailbox")
}
func (s *Session) Select(name string, opts *imap.SelectOptions) (*imap.SelectData, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.Unselect()
	f, err := s.find(name)
	if err != nil {
		return nil, err
	}
	s.folder = f.ID
	s.viewMu.Lock()
	s.tracker = s.b.trackers[f.ID].NewSession()
	s.viewMu.Unlock()
	s.readOnly = opts.ReadOnly
	var unseen uint32
	for i, id := range sortedIDs(f) {
		if contains(s.b.state.Messages[id].Message.LabelIDs, "UNREAD") {
			unseen = uint32(i + 1)
			break
		}
	}
	permanent := supportedFlags
	if opts.ReadOnly {
		permanent = nil
	}
	return &imap.SelectData{Flags: supportedFlags, PermanentFlags: permanent, NumMessages: uint32(len(f.Members)), FirstUnseenSeqNum: unseen, UIDValidity: f.Validity, UIDNext: f.Next}, nil
}
func (s *Session) Create(string, *imap.CreateOptions) error {
	return no("Create labels in Gmail; they appear after the next sync")
}
func (s *Session) Delete(string) error { return no("Delete labels in Gmail") }
func (s *Session) Rename(string, string, *imap.RenameOptions) error {
	return no("Rename labels in Gmail")
}
func (s *Session) Append(string, imap.LiteralReader, *imap.AppendOptions) (*imap.AppendData, error) {
	return nil, no("APPEND is unsupported: use On My Mac drafts and sent storage; Gmail saves sent mail automatically")
}
func (s *Session) Subscribe(name string) error   { return s.subscribe(name, true) }
func (s *Session) Unsubscribe(name string) error { return s.subscribe(name, false) }
func (s *Session) subscribe(name string, value bool) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	f, err := s.find(name)
	if err != nil {
		return err
	}
	next := clone(s.b.state)
	next.Folders[f.ID].Subscribed = value
	return protocolError(s.b.commit(next))
}
func (s *Session) List(w *imapserver.ListWriter, ref string, patterns []string, opts *imap.ListOptions) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if len(patterns) == 0 || len(patterns) == 1 && patterns[0] == "" {
		return w.WriteList(&imap.ListData{Mailbox: "", Delim: '/', Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}})
	}
	var folders []*Folder
	byName := map[string]*Folder{}
	for _, f := range s.b.state.Folders {
		if f.Active {
			folders = append(folders, f)
			byName[f.Name] = f
		}
	}
	// Virtual hierarchy parents are needed by clients that discover folders one
	// level at a time with LIST "%" rather than LIST "*".
	for _, f := range folders {
		name := f.Name
		for strings.Contains(name, "/") {
			name = name[:strings.LastIndex(name, "/")]
			if byName[name] == nil {
				byName[name] = &Folder{Name: name, Active: true}
			}
			if f.Subscribed && byName[name].ID == "" {
				byName[name].Subscribed = true
			}
		}
	}
	folders = nil
	for _, f := range byName {
		folders = append(folders, f)
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].Name < folders[j].Name })
	for _, f := range folders {
		if opts.SelectSubscribed && !f.Subscribed || opts.SelectSpecialUse && f.Attr == "" {
			continue
		}
		match := false
		for _, p := range patterns {
			if imapserver.MatchList(f.Name, '/', ref, p) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		d := &imap.ListData{Mailbox: f.Name, Delim: '/'}
		if f.ID == "" {
			d.Attrs = append(d.Attrs, imap.MailboxAttrNoSelect)
		}
		if f.Attr != "" {
			d.Attrs = append(d.Attrs, f.Attr)
		}
		if opts.ReturnSubscribed && f.Subscribed {
			d.Attrs = append(d.Attrs, imap.MailboxAttrSubscribed)
		}
		if opts.ReturnStatus != nil && f.ID != "" {
			d.Status = s.status(f, opts.ReturnStatus)
		}
		if err := w.WriteList(d); err != nil {
			return err
		}
	}
	return nil
}
func (s *Session) Status(name string, opts *imap.StatusOptions) (*imap.StatusData, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	f, err := s.find(name)
	if err != nil {
		return nil, err
	}
	return s.status(f, opts), nil
}
func (s *Session) status(f *Folder, o *imap.StatusOptions) *imap.StatusData {
	d := &imap.StatusData{Mailbox: f.Name}
	count := uint32(len(f.Members))
	var unseen, deleted, recent uint32
	for id := range f.Members {
		r := s.b.state.Messages[id]
		if contains(r.Message.LabelIDs, "UNREAD") {
			unseen++
		}
		if r.Deleted[f.ID] {
			deleted++
		}
	}
	if o.NumMessages {
		d.NumMessages = &count
	}
	if o.NumRecent {
		d.NumRecent = &recent
	}
	if o.NumUnseen {
		d.NumUnseen = &unseen
	}
	if o.NumDeleted {
		d.NumDeleted = &deleted
	}
	if o.UIDNext {
		d.UIDNext = f.Next
	}
	if o.UIDValidity {
		d.UIDValidity = f.Validity
	}
	return d
}
func (s *Session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	if s.tracker == nil {
		return nil
	}
	return s.tracker.Poll(w, allowExpunge)
}
func (s *Session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	if s.tracker == nil {
		<-stop
		return nil
	}
	// Poll the local update queue only. This avoids a missed notification between
	// draining the queue and registering an IDLE listener in the upstream tracker.
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := s.tracker.Poll(w, true); err != nil {
			return err
		}
		select {
		case <-stop:
			return nil
		case <-ticker.C:
		}
	}
}

type selected struct {
	id  string
	seq uint32
	uid imap.UID
}

func rangeContains(start, stop, n, max uint32) bool {
	if max == 0 || n == 0 {
		return false
	}
	if start == 0 {
		start = max
	}
	if stop == 0 {
		stop = max
	}
	if start > stop {
		start, stop = stop, start
	}
	return n >= start && n <= stop
}
func (s *Session) selected(set imap.NumSet) []selected {
	f := s.b.state.Folders[s.folder]
	ids := sortedIDs(f)
	var out []selected
	var maxSeq uint32
	var maxUID imap.UID
	for i, id := range ids {
		seq := s.tracker.EncodeSeqNum(uint32(i + 1))
		if seq > maxSeq {
			maxSeq = seq
		}
		if seq != 0 && f.Members[id] > maxUID {
			maxUID = f.Members[id]
		}
	}
	for i, id := range ids {
		seq := s.tracker.EncodeSeqNum(uint32(i + 1))
		uid := f.Members[id]
		if seq == 0 {
			continue
		}
		found := false
		switch ns := set.(type) {
		case imap.SeqSet:
			for _, r := range ns {
				found = found || rangeContains(r.Start, r.Stop, seq, maxSeq)
			}
		case imap.UIDSet:
			for _, r := range ns {
				found = found || rangeContains(uint32(r.Start), uint32(r.Stop), uint32(uid), uint32(maxUID))
			}
		}
		if found {
			out = append(out, selected{id, seq, uid})
		}
	}
	return out
}
func (s *Session) Fetch(w *imapserver.FetchWriter, set imap.NumSet, o *imap.FetchOptions) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	ctx, cancel := context.WithTimeout(s.b.ctx, 10*time.Minute)
	defer cancel()
	markSeen := false
	for _, section := range o.BodySection {
		if !section.Peek {
			markSeen = true
		}
	}
	needsRaw := o.RFC822Size || o.Envelope || o.BodyStructure != nil || len(o.BodySection) > 0
	for _, m := range s.selected(set) {
		r := s.b.state.Messages[m.id]
		var raw []byte
		if needsRaw {
			var err error
			raw, err = s.b.raw(ctx, m.id)
			if err != nil {
				return protocolError(err)
			}
		}
		if markSeen && !s.readOnly && contains(r.Message.LabelIDs, "UNREAD") {
			if err := s.b.modify(ctx, m.id, nil, []string{"UNREAD"}); err != nil {
				return protocolError(err)
			}
			r = s.b.state.Messages[m.id]
		}
		// Parse before opening a response: a parse error must not leave the IMAP
		// encoder locked with an unterminated FETCH response.
		var envelope *imap.Envelope
		if o.Envelope {
			h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
			if err != nil {
				return no("Message has malformed MIME headers")
			}
			envelope = imapserver.ExtractEnvelope(h)
		}
		if err := writeFetched(w, m, r, s.folder, raw, envelope, o, markSeen); err != nil {
			return err
		}
	}
	return nil
}
func writeFetched(w *imapserver.FetchWriter, m selected, r *Record, folder string, raw []byte, envelope *imap.Envelope, o *imap.FetchOptions, markSeen bool) (err error) {
	resp := w.CreateMessage(m.seq)
	defer func() {
		if closeErr := resp.Close(); err == nil {
			err = closeErr
		}
	}()
	resp.WriteUID(m.uid)
	if o.Flags || markSeen {
		resp.WriteFlags(flags(r, folder))
	}
	if o.InternalDate {
		ms, _ := strconv.ParseInt(r.Message.InternalDate, 10, 64)
		resp.WriteInternalDate(time.UnixMilli(ms))
	}
	if o.RFC822Size {
		resp.WriteRFC822Size(int64(len(raw)))
	}
	if o.Envelope {
		resp.WriteEnvelope(envelope)
	}
	if o.BodyStructure != nil {
		resp.WriteBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(raw)))
	}
	for _, section := range o.BodySection {
		body := imapserver.ExtractBodySection(bytes.NewReader(raw), section)
		wc := resp.WriteBodySection(section, int64(len(body)))
		_, err := wc.Write(body)
		closeErr := wc.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
func hasFlag(fs []imap.Flag, want imap.Flag) bool {
	for _, f := range fs {
		if strings.EqualFold(string(f), string(want)) {
			return true
		}
	}
	return false
}
func (s *Session) Store(w *imapserver.FetchWriter, set imap.NumSet, op *imap.StoreFlags, _ *imap.StoreOptions) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.readOnly {
		return no("Mailbox is read-only")
	}
	for _, f := range op.Flags {
		if !hasFlag(supportedFlags, f) {
			return no("Unsupported flag")
		}
	}
	ctx, cancel := context.WithTimeout(s.b.ctx, 2*time.Minute)
	defer cancel()
	for _, m := range s.selected(set) {
		r := s.b.state.Messages[m.id]
		old := flags(r, s.folder)
		want := func(f imap.Flag) bool {
			has := hasFlag(op.Flags, f)
			switch op.Op {
			case imap.StoreFlagsSet:
				return has
			case imap.StoreFlagsAdd:
				return hasFlag(old, f) || has
			case imap.StoreFlagsDel:
				return hasFlag(old, f) && !has
			}
			return false
		}
		var add, remove []string
		for _, pair := range []struct {
			flag   imap.Flag
			label  string
			invert bool
		}{{imap.FlagSeen, "UNREAD", true}, {imap.FlagFlagged, "STARRED", false}} {
			present := want(pair.flag) != pair.invert
			if present != contains(r.Message.LabelIDs, pair.label) {
				if present {
					add = append(add, pair.label)
				} else {
					remove = append(remove, pair.label)
				}
			}
		}
		if len(add)+len(remove) > 0 {
			if err := s.b.modify(ctx, m.id, add, remove); err != nil {
				return protocolError(err)
			}
		}
		next := clone(s.b.state)
		r = next.Messages[m.id]
		r.Answered = want(imap.FlagAnswered)
		r.Deleted[s.folder] = want(imap.FlagDeleted)
		if err := s.b.commit(next); err != nil {
			return protocolError(err)
		}
		if !op.Silent {
			resp := w.CreateMessage(m.seq)
			resp.WriteUID(m.uid)
			resp.WriteFlags(flags(r, s.folder))
			if err := resp.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Session) Copy(set imap.NumSet, dest string) (*imap.CopyData, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	f, err := s.find(dest)
	if err != nil {
		return nil, err
	}
	if f.ID == s.folder {
		return nil, no("Source and destination are identical")
	}
	if f.ID == "SENT" {
		return nil, no("Gmail manages the Sent label")
	}
	ctx, cancel := context.WithTimeout(s.b.ctx, 2*time.Minute)
	defer cancel()
	for _, m := range s.selected(set) {
		if f.ID == "TRASH" {
			err = s.b.trash(ctx, m.id)
		} else {
			r := s.b.state.Messages[m.id]
			if contains(r.Message.LabelIDs, "TRASH") {
				if err = s.b.untrash(ctx, m.id); err != nil {
					return nil, protocolError(err)
				}
			}
			var add, remove []string
			if f.ID != allMail {
				add = []string{f.ID}
			}
			if f.ID == "INBOX" || f.ID == allMail {
				remove = []string{"SPAM"}
			}
			if len(add)+len(remove) > 0 {
				err = s.b.modify(ctx, m.id, add, remove)
			}
		}
		if err != nil {
			return nil, protocolError(err)
		}
	}
	return nil, nil // UIDPLUS is deliberately not advertised.
}
func (s *Session) Expunge(_ *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.readOnly {
		return nil
	}
	f := s.b.state.Folders[s.folder]
	var ids []string
	for _, id := range sortedIDs(f) {
		if s.b.state.Messages[id].Deleted[s.folder] && (uids == nil || uids.Contains(f.Members[id])) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if s.folder == "TRASH" {
		return no("Permanent deletion is disabled; empty Trash in Gmail")
	}
	ctx, cancel := context.WithTimeout(s.b.ctx, 2*time.Minute)
	defer cancel()
	for _, id := range ids {
		var err error
		switch s.folder {
		case allMail, "SENT", "SPAM":
			err = s.b.trash(ctx, id)
		default:
			err = s.b.modify(ctx, id, nil, []string{s.folder})
		}
		if err != nil {
			return protocolError(err)
		}
	}
	return nil
}
