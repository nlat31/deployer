package agent

import (
	"deployer/internal/model"
	"deployer/internal/store"
	"errors"
)

// Persist before signalling: a process restart must not resume a cancelled job.
func (e *Engine) RequestCancel(id string) error {
	err := e.DB.Update(func(t *store.Tx) error {
		o, err := store.Get[model.Operation](t, "operations", id)
		if err != nil {
			return err
		}
		if o.Terminal() {
			return nil
		}
		return store.Put(t, "cancel_requests", id, true)
	})
	if err != nil {
		return err
	}
	e.mu.Lock()
	if cancel := e.cancels[id]; cancel != nil {
		cancel()
	}
	e.mu.Unlock()
	return nil
}
func (e *Engine) cancelRequested(id string) (bool, error) {
	requested := false
	err := e.DB.View(func(t *store.Tx) error {
		var err error
		requested, err = store.Get[bool](t, "cancel_requests", id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
	return requested, err
}
