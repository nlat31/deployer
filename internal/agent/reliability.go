package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"deployer/internal/model"
	"deployer/internal/store"
	"deployer/internal/supervisor"
)

// Called under the per-engine mutation lock, after checking pending operations.
func (e *Engine) heal(ctx context.Context, d *model.Deployment, sp supervisor.Spec, st supervisor.Status) error {
	if d.DesiredState != "running" {
		return nil
	}
	now := model.Now()
	if st.Running && d.Health != "unhealthy" {
		if d.HealthySince == 0 {
			d.HealthySince = now
		}
		if now-d.HealthySince >= 60000 {
			d.RestartFailures = 0
			d.NextRestartAt = 0
		}
		d.UnhealthySince = 0
		if strings.HasPrefix(d.Error, "自动恢复") || strings.HasPrefix(d.Error, "进程已自动恢复") {
			d.Error = ""
		}
		return nil
	}
	d.HealthySince = 0
	if st.Running {
		if d.UnhealthySince == 0 {
			d.UnhealthySince = now
		}
		timeout := d.Config.Health.TimeoutSeconds
		if timeout == 0 {
			timeout = 60
		}
		if now-d.UnhealthySince < int64(timeout)*1000 {
			return nil
		}
	}
	if now < d.NextRestartAt {
		return nil
	}
	d.RestartFailures++
	exponent := d.RestartFailures
	if exponent > 6 {
		exponent = 6
	}
	delay := time.Duration(5*(1<<exponent)) * time.Second
	d.NextRestartAt = now + delay.Milliseconds()
	if err := e.DB.Update(func(t *store.Tx) error { return store.Put(t, "deployments", d.AppID, *d) }); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	log := func(string) {}
	if st.Running {
		if err := e.Supervisor.Stop(ctx, sp, log); err != nil {
			d.Error = "自动恢复无法确认进程停止: " + err.Error()
			d.RunState = "unknown"
			return nil
		}
	}
	if err := e.Supervisor.Start(ctx, sp); err != nil {
		d.Error = "自动恢复失败，稍后重试: " + err.Error()
		d.RunState = "stopped"
		d.RunningVersion = ""
		return nil
	}
	next, err := e.Supervisor.Status(ctx, sp)
	if err != nil {
		d.RunState = "unknown"
		d.Error = err.Error()
		return nil
	}
	if next.Running {
		d.RunState = "running"
		d.RunningVersion = d.Current
		d.Health = "unverified"
		d.UnhealthySince = now
		d.Error = "进程已自动恢复，正在等待健康检查"
	}
	return nil
}
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
		return e.finishTx(t, &o, d, "needs_attention", fmt.Sprintf("任务异常，自动恢复未完成: %v", cause))
	})
}
