package store

import (
	"deployer/internal/model"
	"time"
)

// Keep minimal deduplication tombstones; current deployments own their config.
func (s *Store) Maintain() error {
	err := s.Update(func(t *Tx) error {
		if err := Prune(t); err != nil {
			return err
		}
		ops, err := Query[model.Operation](t, "operations", Filter{States: []string{"succeeded", "failed", "failed_recovered", "failed_recovery", "cancelled"}, Before: time.Now().Add(-30 * 24 * time.Hour).UnixMilli(), Limit: 1000})
		if err != nil {
			return err
		}
		for _, o := range ops {
			o.Archived = true
			o.Artifact = nil
			o.Config = model.Config{}
			o.Before = model.Deployment{}
			if err = Put(t, "operations", o.ID, o); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.Checkpoint()
}
