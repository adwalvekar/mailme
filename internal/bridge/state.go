package bridge

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/emersion/go-imap/v2"
	bolt "go.etcd.io/bbolt"
	"mailme/internal/gmail"
)

const allMail = "@all"

type Record struct {
	Message  gmail.Message
	Answered bool
	Deleted  map[string]bool
}
type Folder struct {
	ID         string
	Name       string
	Attr       imap.MailboxAttr
	Active     bool
	Subscribed bool
	Validity   uint32
	Next       imap.UID
	Members    map[string]imap.UID
}
type state struct {
	Email    string
	History  string
	Messages map[string]*Record
	Folders  map[string]*Folder
}
type disk struct{ db *bolt.DB }

func openDisk(dir, email string) (*disk, *state, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, nil, err
	}
	db, err := bolt.Open(filepath.Join(dir, "mail.db"), 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, nil, fmt.Errorf("open mail cache (is another bridge running?): %w", err)
	}
	d := &disk{db}
	st := &state{Email: email, Messages: map[string]*Record{}, Folders: map[string]*Folder{}}
	err = db.Update(func(tx *bolt.Tx) error {
		b, e := tx.CreateBucketIfNotExists([]byte("state"))
		if e != nil {
			return e
		}
		if data := b.Get([]byte("index")); data != nil {
			if e = json.Unmarshal(data, st); e != nil {
				return e
			}
		}
		_, e = tx.CreateBucketIfNotExists([]byte("raw"))
		return e
	})
	if err == nil && st.Email != email {
		err = fmt.Errorf("cache belongs to a different account; use a separate --data-dir")
	}
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return d, st, nil
}
func (d *disk) save(st *state) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte("state")).Put([]byte("index"), data); err != nil {
			return err
		}
		c := tx.Bucket([]byte("raw")).Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			if st.Messages[string(k)] == nil {
				if err := c.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func (d *disk) raw(id string) ([]byte, error) {
	var b []byte
	err := d.db.View(func(tx *bolt.Tx) error { b = append(b, tx.Bucket([]byte("raw")).Get([]byte(id))...); return nil })
	return b, err
}
func (d *disk) putRaw(id string, raw []byte) error {
	return d.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("raw")).Put([]byte(id), raw) })
}
func clone(st *state) *state {
	b, _ := json.Marshal(st)
	var next state
	_ = json.Unmarshal(b, &next)
	return &next
}
func randomValidity() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	n := binary.BigEndian.Uint32(b[:])
	if n == 0 {
		n = 1
	}
	return n
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func flags(r *Record, folder string) []imap.Flag {
	var f []imap.Flag
	if !contains(r.Message.LabelIDs, "UNREAD") {
		f = append(f, imap.FlagSeen)
	}
	if contains(r.Message.LabelIDs, "STARRED") {
		f = append(f, imap.FlagFlagged)
	}
	if r.Answered {
		f = append(f, imap.FlagAnswered)
	}
	if r.Deleted[folder] {
		f = append(f, imap.FlagDeleted)
	}
	return f
}
func sortedIDs(f *Folder) []string {
	ids := make([]string, 0, len(f.Members))
	for id := range f.Members {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return f.Members[ids[i]] < f.Members[ids[j]] })
	return ids
}
func inFolder(r *Record, id string) bool {
	labels := r.Message.LabelIDs
	// Drafts are edited locally in this version; do not expose read-only draft copies.
	if contains(labels, "DRAFT") {
		return false
	}
	if id == allMail {
		return !contains(labels, "TRASH") && !contains(labels, "SPAM")
	}
	if id != "TRASH" && id != "SPAM" && (contains(labels, "TRASH") || contains(labels, "SPAM")) {
		return false
	}
	return contains(labels, id)
}
func (st *state) setLabels(labels []gmail.Label) {
	for _, f := range st.Folders {
		f.Active = false
	}
	add := func(id, name string, attr imap.MailboxAttr) {
		f := st.Folders[id]
		if f == nil {
			f = &Folder{ID: id, Validity: randomValidity(), Next: 1, Subscribed: true, Members: map[string]imap.UID{}}
			st.Folders[id] = f
		}
		f.Name = name
		f.Attr = attr
		f.Active = true
	}
	add("INBOX", "INBOX", "")
	add("SENT", "Gmail/Sent", imap.MailboxAttrSent)
	add("TRASH", "Gmail/Trash", imap.MailboxAttrTrash)
	add("SPAM", "Gmail/Junk", imap.MailboxAttrJunk)
	add("STARRED", "Gmail/Starred", imap.MailboxAttrFlagged)
	add(allMail, "Gmail/All Mail", imap.MailboxAttrAll)
	for _, l := range labels {
		if l.Type == "user" {
			add(l.ID, "Labels/"+l.Name, "")
		}
	}
}

// Preserve UIDs for surviving memberships; a removed/re-added message gets a
// new UID. Deterministic date order makes first sync pleasant in mail clients.
func (st *state) reconcile() error {
	ids := make([]string, 0, len(st.Messages))
	for id := range st.Messages {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := st.Messages[ids[i]].Message, st.Messages[ids[j]].Message
		if a.InternalDate == b.InternalDate {
			return a.ID < b.ID
		}
		return len(a.InternalDate) < len(b.InternalDate) || len(a.InternalDate) == len(b.InternalDate) && a.InternalDate < b.InternalDate
	})
	for fid, f := range st.Folders {
		for id := range f.Members {
			r := st.Messages[id]
			if !f.Active || r == nil || !inFolder(r, fid) {
				delete(f.Members, id)
				if r != nil {
					delete(r.Deleted, fid)
				}
			}
		}
		if !f.Active {
			continue
		}
		for _, id := range ids {
			if inFolder(st.Messages[id], fid) && f.Members[id] == 0 {
				if f.Next == 0 || f.Next == ^imap.UID(0) {
					return fmt.Errorf("IMAP UID space exhausted")
				}
				f.Members[id] = f.Next
				f.Next++
			}
		}
	}
	return nil
}
