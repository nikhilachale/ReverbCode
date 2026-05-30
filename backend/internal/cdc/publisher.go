package cdc

import (
	"context"
	"log/slog"
	"time"
)

// DefaultPublishInterval is the outbox drain cadence.
const DefaultPublishInterval = 50 * time.Millisecond

// DefaultBatchSize bounds how many outbox rows one drain pass handles.
const DefaultBatchSize = 256

// maxOutboxAttempts is the cap after which Drain stops re-trying a wedged
// outbox row in-stream. Beyond this threshold the row stays in the outbox
// (operator-visible via outbox.attempts and outbox.last_error) but Drain
// continues past it so a single poisoned row cannot block the entire
// downstream pipeline. Manual intervention (delete the row, fix the underlying
// log/disk issue) is then required.
const maxOutboxAttempts = 5

// PendingEvent is an undelivered outbox row paired with its CDC event payload.
// Attempts is the row's current outbox.attempts counter (it reflects past
// failures and is not yet incremented for the in-progress drain pass); Drain
// reads it to decide whether to keep re-trying or step past a wedged row.
type PendingEvent struct {
	OutboxID int64
	Attempts int64
	Event
}

// OutboxStore is the publisher's view of the storage layer: read undelivered
// rows in seq order, then mark each delivered or failed.
type OutboxStore interface {
	ListUnsent(ctx context.Context, limit int) ([]PendingEvent, error)
	MarkSent(ctx context.Context, outboxID int64, at time.Time) error
	MarkFailed(ctx context.Context, outboxID int64, errMsg string) error
}

// logAppender is the minimum Publisher needs from a JSONL log: appending one
// event in seq order. Production wires *Log; tests can inject a fault-injecting
// implementation to exercise the wedged-outbox-row path (S2).
type logAppender interface {
	Append(e Event) error
}

// Publisher drains the outbox into the JSONL log on a fixed cadence.
type Publisher struct {
	src      OutboxStore
	log      logAppender
	interval time.Duration
	batch    int
	clock    func() time.Time
	logger   *slog.Logger
}

// PublisherConfig holds optional knobs; zero values fall back to defaults.
type PublisherConfig struct {
	Interval time.Duration
	Batch    int
	Clock    func() time.Time
	Logger   *slog.Logger
}

// NewPublisher constructs a Publisher over src and log. log may be any
// logAppender; production passes *Log directly.
func NewPublisher(src OutboxStore, log logAppender, cfg PublisherConfig) *Publisher {
	p := &Publisher{
		src:      src,
		log:      log,
		interval: cfg.Interval,
		batch:    cfg.Batch,
		clock:    cfg.Clock,
		logger:   cfg.Logger,
	}
	if p.interval <= 0 {
		p.interval = DefaultPublishInterval
	}
	if p.batch <= 0 {
		p.batch = DefaultBatchSize
	}
	if p.clock == nil {
		p.clock = time.Now
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	return p
}

// Start runs the drain loop until ctx is cancelled; the returned channel closes
// when the loop has exited.
func (p *Publisher) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := p.Drain(ctx); err != nil {
					p.logger.Error("cdc publisher: drain failed", "err", err)
				}
			}
		}
	}()
	return done
}

// Drain runs one pass: append each undelivered row to the log in seq order,
// marking it sent. When append fails the row is MarkFailed (attempts++,
// last_error stamped) and Drain ordinarily stops the pass so a transient
// failure doesn't reorder the stream. Once a row crosses maxOutboxAttempts
// Drain logs a warning and continues past it instead of returning — without
// that escape hatch a single permanently-poisoned row wedges the pipeline
// forever with no operator surface beyond outbox.last_error.
func (p *Publisher) Drain(ctx context.Context) error {
	pending, err := p.src.ListUnsent(ctx, p.batch)
	if err != nil {
		return err
	}
	for _, pe := range pending {
		if err := p.log.Append(pe.Event); err != nil {
			p.logger.Error("cdc publisher: append failed",
				"outboxId", pe.OutboxID, "seq", pe.Seq, "attempts", pe.Attempts, "err", err)
			if merr := p.src.MarkFailed(ctx, pe.OutboxID, err.Error()); merr != nil {
				p.logger.Error("cdc publisher: mark failed errored", "outboxId", pe.OutboxID, "err", merr)
			}
			// pe.Attempts is the *pre-increment* count from ListUnsent. After
			// the MarkFailed above, the persistent count is pe.Attempts + 1.
			// Skip past the row when that post-increment value has hit the
			// threshold; the row stays in the outbox so an operator can find
			// and clear it.
			if pe.Attempts+1 >= maxOutboxAttempts {
				p.logger.Warn("cdc publisher: outbox row exceeded attempts threshold; skipping in-stream until manually cleared",
					"outboxId", pe.OutboxID, "seq", pe.Seq, "attempts", pe.Attempts+1, "lastError", err.Error())
				continue
			}
			return nil
		}
		if err := p.src.MarkSent(ctx, pe.OutboxID, p.clock().UTC()); err != nil {
			return err
		}
	}
	return nil
}
