package agent

import (
	"fmt"

	"deployer/internal/model"
	"deployer/internal/store"
)

func (e *Engine) markAttention(id string, cause error) error {
	return e.DB.Update(func(t *store.Tx) error {
		o, err := store.Get[model.Operation](t, "operations", id)
		if err != nil {
			return err
		}
		if o.Terminal() {
			return nil
		}
		d, err := store.Get[model.Deployment](t, "deployments", o.AppID)
		if err != nil {
			return err
		}
		d.DesiredState = "stopped"
		d.RunState = "unknown"
		d.Health = "unknown"
		return e.finishTx(t, &o, d, "needs_attention", fmt.Sprintf("任务异常，需管理员核对: %v", cause))
	})
}
