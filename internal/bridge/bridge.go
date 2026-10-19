package bridge

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2/imapserver"
	"mailme/internal/gmail"
)

type API interface {
	Profile(context.Context) (*gmail.Profile, error)
	Labels(context.Context) ([]gmail.Label, error)
	IDs(context.Context) ([]string, error)
	Message(context.Context, string, string) (*gmail.Message, error)
	Changes(context.Context, string) ([]string, string, error)
	Modify(context.Context, string, []string, []string) (*gmail.Message, error)
	Trash(context.Context, string) (*gmail.Message, error)
	Untrash(context.Context, string) (*gmail.Message, error)
	Send(context.Context, []byte) error
}

type Bridge struct {
	// Serialize synchronization and mutations so an older API snapshot cannot
	// overwrite a successful STORE, and sequence numbers cannot shift mid-command.
	mu       sync.Mutex
	api      API
	disk     *disk
	state    *state
	trackers map[string]*imapserver.MailboxTracker
	password [32]byte
	email    string
	ctx      context.Context
	cancel   context.CancelFunc
}

func Open(dir, email, password string, api API) (*Bridge, error) {
	if len(password) < 24 {
		return nil, fmt.Errorf("bridge password must contain at least 24 characters")
	}
	email = strings.ToLower(email)
	d, st, err := openDisk(dir, email)
	if err != nil {
		return nil, err
	}
	b := &Bridge{api: api, disk: d, state: st, email: email, password: sha256.Sum256([]byte(password)), trackers: map[string]*imapserver.MailboxTracker{}}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	for id, f := range st.Folders {
		b.trackers[id] = imapserver.NewMailboxTracker(uint32(len(f.Members)))
	}
	return b, nil
}
func (b *Bridge) Stop()        { b.cancel() }
func (b *Bridge) Close() error { b.Stop(); b.mu.Lock(); defer b.mu.Unlock(); return b.disk.db.Close() }
func (b *Bridge) Authenticate(user, password string) bool {
	h := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(h[:], b.password[:]) == 1 && strings.EqualFold(user, b.email)
}
func (b *Bridge) commit(next *state) error {
	if err := next.reconcile(); err != nil {
		return err
	}
	if err := b.disk.save(next); err != nil {
		return err
	}
	for fid, f := range next.Folders {
		t := b.trackers[fid]
		if t == nil {
			b.trackers[fid] = imapserver.NewMailboxTracker(uint32(len(f.Members)))
			continue
		}
		old := b.state.Folders[fid]
		oldIDs := sortedIDs(old)
		for i := len(oldIDs) - 1; i >= 0; i-- {
			id := oldIDs[i]
			if f.Members[id] == 0 {
				t.QueueExpunge(uint32(i + 1))
			}
		}
		survivors := 0
		for _, id := range oldIDs {
			if f.Members[id] != 0 {
				survivors++
			}
		}
		// The library's session tracker expects individual EXISTS increments.
		for n := survivors + 1; n <= len(f.Members); n++ {
			t.QueueNumMessages(uint32(n))
		}
		for i, id := range sortedIDs(f) {
			before := b.state.Messages[id]
			after := next.Messages[id]
			if before == nil || old.Members[id] == 0 || !reflect.DeepEqual(flags(before, fid), flags(after, fid)) {
				t.QueueMessageFlags(uint32(i+1), f.Members[id], flags(after, fid), nil)
			}
		}
	}
	b.state = next
	return nil
}
func is404(err error) bool { var e *gmail.APIError; return errors.As(err, &e) && e.Status == 404 }

func (b *Bridge) Sync(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	labels, err := b.api.Labels(ctx)
	if err != nil {
		return err
	}
	next := clone(b.state)
	next.setLabels(labels)
	var ids []string
	var history string
	full := next.History == ""
	if !full {
		ids, history, err = b.api.Changes(ctx, next.History)
		if is404(err) {
			full = true
		} else if err != nil {
			return err
		}
	}
	if full {
		// Capture the cursor BEFORE listing: a subsequent delta catches mutations
		// that happen while the full snapshot is being collected.
		p, err := b.api.Profile(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(p.Email, b.email) {
			return fmt.Errorf("OAuth account does not match configured email")
		}
		history = p.HistoryID
		ids, err = b.api.IDs(ctx)
		if err != nil {
			return err
		}
		present := map[string]bool{}
		for _, id := range ids {
			present[id] = true
		}
		for id := range next.Messages {
			if !present[id] {
				delete(next.Messages, id)
			}
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		m, err := b.api.Message(ctx, id, "metadata")
		if is404(err) {
			delete(next.Messages, id)
			continue
		}
		if err != nil {
			return err
		}
		putRecord(next, m)
	}
	next.History = history
	return b.commit(next)
}
func putRecord(st *state, m *gmail.Message) {
	r := st.Messages[m.ID]
	if r == nil {
		r = &Record{Deleted: map[string]bool{}}
		st.Messages[m.ID] = r
	}
	date := r.Message.InternalDate
	r.Message = *m
	r.Message.Raw = ""
	if r.Message.InternalDate == "" {
		r.Message.InternalDate = date
	}
}
func (b *Bridge) RunSync(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := b.Sync(ctx); err != nil && ctx.Err() == nil {
				log.Printf("Gmail sync failed; retaining last complete snapshot: %v", err)
			}
		}
	}
}
func (b *Bridge) raw(ctx context.Context, id string) ([]byte, error) {
	raw, err := b.disk.raw(id)
	if err != nil || len(raw) > 0 {
		return raw, err
	}
	m, err := b.api.Message(ctx, id, "raw")
	if err != nil {
		return nil, err
	}
	raw, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(m.Raw, "="))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("Gmail returned empty message content")
	}
	if err = b.disk.putRaw(id, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// Mutation responses do not advance the history checkpoint: doing so could skip
// unrelated changes made by another client.
func (b *Bridge) modify(ctx context.Context, id string, add, remove []string) error {
	m, err := b.api.Modify(ctx, id, add, remove)
	if err != nil {
		return err
	}
	next := clone(b.state)
	putRecord(next, m)
	return b.commit(next)
}
func (b *Bridge) trash(ctx context.Context, id string) error {
	m, err := b.api.Trash(ctx, id)
	if err != nil {
		return err
	}
	next := clone(b.state)
	putRecord(next, m)
	return b.commit(next)
}
func (b *Bridge) untrash(ctx context.Context, id string) error {
	m, err := b.api.Untrash(ctx, id)
	if err != nil {
		return err
	}
	next := clone(b.state)
	putRecord(next, m)
	return b.commit(next)
}
