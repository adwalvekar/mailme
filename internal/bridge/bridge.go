package bridge

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

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
	// mu protects immutable state snapshots and commits; never hold it for API I/O.
	mu         sync.Mutex
	syncMu     sync.Mutex
	mutations  sync.Mutex // order complete local mutation commands, independently of readers
	versions   map[string]uint64
	workers    int
	batchSize  int
	rawMu      sync.Mutex
	rawFlights map[string]*rawFlight
	rawWG      sync.WaitGroup
	closing    bool
	api        API
	disk       *disk
	state      *state
	trackers   map[string]*imapserver.MailboxTracker
	password   [32]byte
	email      string
	ctx        context.Context
	cancel     context.CancelFunc
}

type Options struct {
	Workers   int
	BatchSize int
}

func Open(dir, email, password string, api API, options ...Options) (*Bridge, error) {
	o := Options{Workers: 6, BatchSize: 250}
	if len(options) > 0 {
		if options[0].Workers != 0 {
			o.Workers = options[0].Workers
		}
		if options[0].BatchSize != 0 {
			o.BatchSize = options[0].BatchSize
		}
	}
	if o.Workers < 1 || o.Workers > 16 || o.BatchSize < 1 || o.BatchSize > 500 {
		return nil, fmt.Errorf("invalid sync worker or checkpoint batch size")
	}
	if len(password) < 24 {
		return nil, fmt.Errorf("bridge password must contain at least 24 characters")
	}
	email = strings.ToLower(email)
	d, st, err := openDisk(dir, email)
	if err != nil {
		return nil, err
	}
	b := &Bridge{api: api, disk: d, state: st, email: email, password: sha256.Sum256([]byte(password)), trackers: map[string]*imapserver.MailboxTracker{}}
	b.workers, b.batchSize = o.Workers, o.BatchSize
	b.versions = make(map[string]uint64)
	b.rawFlights = make(map[string]*rawFlight)
	b.ctx, b.cancel = context.WithCancel(context.Background())
	for id, f := range st.Folders {
		b.trackers[id] = imapserver.NewMailboxTracker(uint32(len(f.Members)))
	}
	// Persist even an empty INBOX's UIDVALIDITY before clients can select it.
	if err := d.save(st); err != nil {
		b.cancel()
		d.db.Close()
		return nil, err
	}
	return b, nil
}
func (b *Bridge) Stop() { b.cancel() }
func (b *Bridge) Close() error {
	b.Stop()
	b.rawMu.Lock()
	b.closing = true
	b.rawMu.Unlock()
	b.rawWG.Wait()
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	b.mutations.Lock()
	defer b.mutations.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.disk.db.Close()
}
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

func putRecord(st *state, m *gmail.Message) {
	r := st.Messages[m.ID]
	if r == nil {
		r = &Record{Deleted: map[string]bool{}}
		st.Messages[m.ID] = r
	}
	date := r.Message.InternalDate
	payload, thread := r.Message.Payload, r.Message.ThreadID
	r.Message = *m
	r.Message.Raw = ""
	if r.Message.InternalDate == "" {
		r.Message.InternalDate = date
	}
	if r.Message.Payload == nil {
		r.Message.Payload = payload
	}
	if r.Message.ThreadID == "" {
		r.Message.ThreadID = thread
	}
}
