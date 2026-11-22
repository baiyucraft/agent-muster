package work

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
)

func startLease(ctx context.Context, run control.Run, peer *collaboration, cancel context.CancelCauseFunc) error {
	if peer == nil {
		return errors.New("设备端无法维护运行租约")
	}
	until, err := time.Parse(time.RFC3339Nano, run.LeaseUntil)
	if err != nil {
		return errors.New("Hub 返回的运行租约时间无效")
	}
	if !until.After(time.Now()) {
		return errors.New("运行租约已经过期")
	}
	proof := control.LeaseProof{Generation: run.Generation, Lease: run.Lease}
	go func() {
		defer cancel(nil)
		for ctx.Err() == nil {
			remaining := time.Until(until)
			if remaining <= 0 {
				cancel(errors.New("运行租约已经过期"))
				return
			}
			if !sleepContext(ctx, min(15*time.Second, remaining/2)) {
				return
			}
			renewCtx, stop := context.WithDeadline(ctx, until)
			next, err := peer.client.RenewRun(renewCtx, peer.identity, run.ID, proof)
			stop()
			var status *control.HTTPError
			if errors.Is(err, control.ErrUnauthorized) || errors.As(err, &status) && (status.Status == http.StatusConflict || status.Status == http.StatusNotFound) {
				cancel(errors.New("运行租约已失效"))
				return
			}
			if err == nil && next.After(until) {
				until = next
			}
		}
	}()
	return nil
}
