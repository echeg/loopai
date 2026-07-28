package status

import "sync"

// PhaseHolder stores the current execution phase in a thread-safe way.
// it is the single source of truth for the current phase across all components.
type PhaseHolder struct {
	mu        sync.RWMutex
	phase     Phase
	observers []func(old, cur Phase)
}

// OnChange registers a callback that fires when the phase changes.
// multiple observers are supported and fire in registration order; a nil callback is ignored.
func (h *PhaseHolder) OnChange(fn func(old, cur Phase)) {
	if fn == nil {
		return
	}
	h.mu.Lock()
	h.observers = append(h.observers, fn)
	h.mu.Unlock()
}

// Set updates the current phase and fires the registered observers if the phase changed.
// observers are invoked outside the lock so they may call back into the holder.
func (h *PhaseHolder) Set(p Phase) {
	h.mu.Lock()
	old := h.phase
	h.phase = p
	// snapshot under the lock: a concurrent OnChange may append and reallocate the slice.
	// only taken when the phase actually changed, so repeated Set of the same phase never allocates.
	var observers []func(old, cur Phase)
	if old != p {
		observers = make([]func(old, cur Phase), len(h.observers))
		copy(observers, h.observers)
	}
	h.mu.Unlock()

	for _, fn := range observers {
		fn(old, p)
	}
}

// Get returns the current phase.
func (h *PhaseHolder) Get() Phase {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.phase
}
