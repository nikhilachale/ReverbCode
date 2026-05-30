package cdc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeAppender is a logAppender that fails Append until the test flips its
// switch. Used to drive the S2 wedged-outbox-row path deterministically.
type fakeAppender struct {
	mu       sync.Mutex
	fail     bool
	appended []Event
}

func (f *fakeAppender) Append(e Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("simulated append failure")
	}
	f.appended = append(f.appended, e)
	return nil
}

func (f *fakeAppender) Appended() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Event, len(f.appended))
	copy(out, f.appended)
	return out
}

// fakeOutbox is an OutboxStore backed by an in-memory slice so we can assert
// attempts counters and ordering without dragging in SQLite.
type fakeOutbox struct {
	mu   sync.Mutex
	rows []*fakeRow
}

type fakeRow struct {
	id       int64
	event    Event
	attempts int64
	sent     bool
	lastErr  string
}

func (f *fakeOutbox) add(e Event) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := int64(len(f.rows) + 1)
	f.rows = append(f.rows, &fakeRow{id: id, event: e})
	return id
}

func (f *fakeOutbox) ListUnsent(_ context.Context, limit int) ([]PendingEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PendingEvent, 0, len(f.rows))
	for _, r := range f.rows {
		if r.sent {
			continue
		}
		out = append(out, PendingEvent{
			OutboxID: r.id,
			Attempts: r.attempts,
			Event:    r.event,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeOutbox) MarkSent(_ context.Context, id int64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.id == id {
			r.sent = true
			return nil
		}
	}
	return errors.New("row not found")
}

func (f *fakeOutbox) MarkFailed(_ context.Context, id int64, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.id == id {
			r.attempts++
			r.lastErr = msg
			return nil
		}
	}
	return errors.New("row not found")
}

func (f *fakeOutbox) row(id int64) *fakeRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.id == id {
			return r
		}
	}
	return nil
}

// TestPublisherSkipsPermanentlyFailingRow pins the S2 fix. A row whose
// log.Append always fails was previously a permanent wedge: Drain MarkFailed'd
// the row and returned, ListUnsent returned the same row first next tick, and
// the pipeline stalled forever with no DLQ. After the fix, once attempts
// crosses maxOutboxAttempts Drain logs a warning and steps past the row so
// subsequent events continue flowing.
func TestPublisherSkipsPermanentlyFailingRow(t *testing.T) {
	ctx := context.Background()
	box := &fakeOutbox{}
	app := &fakeAppender{fail: true}

	doomedID := box.add(Event{Seq: 1, SessionID: "doomed", EventType: "session_created"})

	pub := NewPublisher(box, app, PublisherConfig{})

	// Drain repeatedly. The first (maxOutboxAttempts-1) passes mark the row
	// failed and return (no skip yet). The maxOutboxAttempts-th pass crosses
	// the threshold and skips past — but since there is no other row, it's a
	// no-op continuation.
	for i := 0; i < maxOutboxAttempts; i++ {
		if err := pub.Drain(ctx); err != nil {
			t.Fatalf("drain %d: %v", i, err)
		}
	}

	r := box.row(doomedID)
	if r == nil {
		t.Fatal("doomed row vanished")
	}
	if r.sent {
		t.Fatal("doomed row was marked sent despite append failure")
	}
	if r.attempts != int64(maxOutboxAttempts) {
		t.Fatalf("doomed row attempts = %d, want %d", r.attempts, maxOutboxAttempts)
	}
	if r.lastErr == "" {
		t.Fatal("doomed row last_error not stamped")
	}

	// Seed a follow-up event; flip the appender to succeed for it (the doomed
	// row is still in the list and would still fail on Append, but it's now
	// past the threshold so Drain must skip past it and reach the good row).
	okID := box.add(Event{Seq: 2, SessionID: "ok", EventType: "session_created"})

	// Drain once. The doomed row's append will still fail (appender is still
	// failing), but it should be skipped because attempts >= threshold. To
	// distinguish "skip past doomed" from "doomed still wedges": switch the
	// appender to succeed-on-non-doomed by inspecting the event id. Simpler:
	// keep failing on doomed (seq 1) but succeed on others.
	app.mu.Lock()
	app.fail = false
	app.mu.Unlock()
	// Now make Append fail selectively for seq 1.
	pub2 := NewPublisher(box, &selectiveFailAppender{failSeq: 1, inner: app}, PublisherConfig{})
	if err := pub2.Drain(ctx); err != nil {
		t.Fatalf("post-threshold drain: %v", err)
	}

	// "ok" row should now be sent; "doomed" should still be unsent in the
	// outbox (operator must clear manually) but its attempts increased again.
	if got := box.row(okID); got == nil || !got.sent {
		t.Fatalf("ok row not delivered; got=%+v", got)
	}
	if got := box.row(doomedID); got == nil || got.sent {
		t.Fatalf("doomed row should remain unsent; got=%+v", got)
	}
}

// selectiveFailAppender fails Append for a chosen seq and delegates the rest
// to the inner appender. Used to assert the publisher "skips past" a wedged
// row to deliver healthy downstream rows in the same Drain pass.
type selectiveFailAppender struct {
	failSeq int64
	inner   *fakeAppender
}

func (s *selectiveFailAppender) Append(e Event) error {
	if e.Seq == s.failSeq {
		return errors.New("simulated permanent failure for seq=" + string(rune(s.failSeq)))
	}
	return s.inner.Append(e)
}
