package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"mailme/internal/gmail"
)

type probeAPI interface {
	Profile(context.Context) (*gmail.Profile, error)
	IDs(context.Context) ([]string, error)
	Message(context.Context, string, string) (*gmail.Message, error)
}

// This path never opens mail.db, asks for the local password, or invokes a write
// endpoint. Only counts, request timing, opaque IDs and sanitized errors leave it.
func probe(ctx context.Context, api probeAPI, id string, start, count, workers int) error {
	if _, err := api.Profile(ctx); err != nil {
		return err
	}
	ids := []string{id}
	if id == "" {
		var err error
		ids, err = api.IDs(ctx)
		if err != nil {
			return err
		}
		sort.Strings(ids)
		if start >= len(ids) {
			return fmt.Errorf("start index is outside the %d-message listing", len(ids))
		}
		ids = ids[start:]
		if len(ids) > count {
			ids = ids[:count]
		}
	} else {
		start = 0
	}
	type result struct {
		index int
		err   error
	}
	jobs := make(chan int)
	results := make(chan result, workers)
	var wg sync.WaitGroup
	t0 := time.Now()
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				_, err := api.Message(ctx, ids[index], "metadata")
				results <- result{index, err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := range ids {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	completed, failed := 0, 0
	for r := range results {
		completed++
		if r.err != nil {
			failed++
			log.Printf("Probe failed index=%d id=%s: %v", start+r.index, gmail.SafeIdentifier(ids[r.index]), r.err)
		}
	}
	duration := time.Since(t0)
	log.Printf("Read-only probe: fetched=%d failed=%d readers=%d elapsed=%s rate=%.2f messages/s (excludes listing)", completed, failed, workers, duration.Round(time.Millisecond), float64(completed)/duration.Seconds())
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed > 0 {
		return fmt.Errorf("probe encountered %d failed message reads", failed)
	}
	return nil
}
