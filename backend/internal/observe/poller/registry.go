package poller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Poller is the common daemon start/stop seam for background observers. Each
// poller owns its own cadence internally; the registry just starts all of them
// under the daemon context and waits for clean shutdown.
type Poller interface {
	Name() string
	Start(context.Context) <-chan struct{}
}

type Func struct {
	PollerName string
	StartFunc  func(context.Context) <-chan struct{}
}

func (f Func) Name() string { return f.PollerName }

func (f Func) Start(ctx context.Context) <-chan struct{} {
	if f.StartFunc == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return f.StartFunc(ctx)
}

type Registry struct {
	logger  *slog.Logger
	pollers []Poller
	done    []<-chan struct{}
}

func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{logger: logger}
}

func (r *Registry) Register(p Poller) error {
	if p == nil {
		return nil
	}
	name := p.Name()
	if name == "" {
		return fmt.Errorf("poller name is required")
	}
	for _, existing := range r.pollers {
		if existing.Name() == name {
			return fmt.Errorf("poller %q already registered", name)
		}
	}
	r.pollers = append(r.pollers, p)
	return nil
}

func (r *Registry) Start(ctx context.Context) {
	for _, p := range r.pollers {
		r.logger.Debug("poller starting", "name", p.Name())
		r.done = append(r.done, p.Start(ctx))
	}
}

func (r *Registry) Stop() {
	var wg sync.WaitGroup
	for _, ch := range r.done {
		wg.Add(1)
		go func(done <-chan struct{}) {
			defer wg.Done()
			<-done
		}(ch)
	}
	wg.Wait()
}
