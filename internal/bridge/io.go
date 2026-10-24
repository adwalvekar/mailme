package bridge

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"mailme/internal/gmail"
)

type rawFlight struct {
	done chan struct{}
	raw  []byte
	err  error
}

func (b *Bridge) raw(ctx context.Context, id string) ([]byte, error) {
	b.rawMu.Lock()
	if b.closing {
		b.rawMu.Unlock()
		return nil, context.Canceled
	}
	f := b.rawFlights[id]
	if f == nil {
		f = &rawFlight{done: make(chan struct{})}
		b.rawFlights[id] = f
		b.rawWG.Add(1)
		go func() {
			defer b.rawWG.Done()
			// A cancelled waiter does not cancel a fetch shared by other sessions.
			fetchCtx, cancel := context.WithTimeout(b.ctx, time.Minute)
			defer cancel()
			f.raw, f.err = b.loadRaw(fetchCtx, id)
			b.rawMu.Lock()
			delete(b.rawFlights, id)
			close(f.done)
			b.rawMu.Unlock()
		}()
	}
	b.rawMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		return f.raw, f.err
	}
}

func (b *Bridge) loadRaw(ctx context.Context, id string) ([]byte, error) {
	raw, err := b.disk.raw(id)
	if err != nil || len(raw) > 0 {
		return raw, err
	}
	m, err := b.api.Message(ctx, id, "raw")
	if err != nil {
		return nil, err
	}
	if err := gmail.ValidateMessage(m, id, "raw"); err != nil {
		return nil, err
	}
	raw, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(m.Raw, "="))
	if err != nil || len(raw) == 0 {
		return nil, &gmail.MalformedMessageError{ID: id}
	}
	if err = b.disk.putRaw(id, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (b *Bridge) record(id string) *Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.Messages[id] // committed Records are immutable
}

// Callers hold mutations for their complete IMAP command, never b.mu. Bump both
// before and after I/O so a read overlapping either side cannot clobber it.
func (b *Bridge) mutate(ctx context.Context, id string, request func() (*gmail.Message, error)) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	b.mu.Lock()
	b.versions[id]++
	b.mu.Unlock()
	m, err := request()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.versions[id]++
	if err != nil {
		return err
	}
	if m == nil || m.ID != id {
		return fmt.Errorf("invalid Gmail mutation response id=%s", gmail.SafeIdentifier(id))
	}
	next := clone(b.state)
	if next.Pending != nil {
		next.Pending.Touched[id] = true
	}
	putRecord(next, m)
	delete(next.Skipped, id)
	return b.commit(next)
}

func (b *Bridge) modify(ctx context.Context, id string, add, remove []string) error {
	return b.mutate(ctx, id, func() (*gmail.Message, error) { return b.api.Modify(ctx, id, add, remove) })
}
func (b *Bridge) trash(ctx context.Context, id string) error {
	return b.mutate(ctx, id, func() (*gmail.Message, error) { return b.api.Trash(ctx, id) })
}
func (b *Bridge) untrash(ctx context.Context, id string) error {
	return b.mutate(ctx, id, func() (*gmail.Message, error) { return b.api.Untrash(ctx, id) })
}
