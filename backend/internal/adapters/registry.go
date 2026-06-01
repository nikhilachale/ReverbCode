// Package adapters defines the plugin contract every external integration
// (agent, tracker, scm, runtime) satisfies plus a registry that holds the
// concrete plugins the daemon resolves by id.
package adapters

import (
	"fmt"
	"sort"
)

// Capability tags a Manifest with the role(s) a plugin fills.
type Capability string

// Known capabilities. A plugin may advertise more than one.
const (
	CapabilityAgent        Capability = "agent"
	CapabilityIssueTracker Capability = "issue-tracker"
)

// Manifest is the self-describing record every Adapter returns.
type Manifest struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Version      string       `json:"version"`
	Capabilities []Capability `json:"capabilities"`
}

// Adapter is the minimal contract every registered plugin satisfies: it can
// describe itself via Manifest. Per-capability behaviour lives on richer
// interfaces (e.g. agent.Agent) that callers obtain via type assertion.
type Adapter interface {
	Manifest() Manifest
}

// Registry holds the daemon's resolved plugins, keyed by Manifest.ID.
type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry returns an empty Registry ready to accept Register calls.
func NewRegistry() *Registry {
	return &Registry{
		adapters: make(map[string]Adapter),
	}
}

// Register adds adapter under its Manifest.ID, returning an error when the id
// is empty or already in use.
func (r *Registry) Register(adapter Adapter) error {
	manifest := adapter.Manifest()
	if manifest.ID == "" {
		return fmt.Errorf("adapter id is required")
	}
	if _, exists := r.adapters[manifest.ID]; exists {
		return fmt.Errorf("adapter %q is already registered", manifest.ID)
	}

	r.adapters[manifest.ID] = adapter
	return nil
}

// Get returns the registered adapter with the given id, or nil and false
// when no such adapter exists.
func (r *Registry) Get(id string) (Adapter, bool) {
	p, ok := r.adapters[id]
	return p, ok
}

// Manifests returns every registered adapter's Manifest, sorted by id for
// deterministic output.
func (r *Registry) Manifests() []Manifest {
	manifests := make([]Manifest, 0, len(r.adapters))
	for _, adapter := range r.adapters {
		manifests = append(manifests, adapter.Manifest())
	}

	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].ID < manifests[j].ID
	})

	return manifests
}
