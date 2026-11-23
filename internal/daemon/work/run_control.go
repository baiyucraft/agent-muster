package work

import (
	"context"
	"errors"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
)

const runControlInterval = 2 * time.Second

var errRunStopRequested = errors.New("Hub 已请求停止本轮")

// startRunControl 先检查本地已领取的运行，再独立轮询停止指令。
// 只有辨认出该运行的停止指令时才取消运行。短暂网络错误由后续检查处理。
func startRunControl(ctx context.Context, run control.Run, peer *collaboration, cancel context.CancelCauseFunc) {
	if !run.ControlSupported || peer == nil || run.Lease == "" {
		return
	}
	proof := control.LeaseProof{Generation: run.Generation, Lease: run.Lease}
	check := func() bool {
		request, done := context.WithTimeout(ctx, runControlInterval)
		defer done()
		stop, err := peer.client.RunStopRequested(request, peer.identity, run.ID, proof)
		if err == nil && stop {
			cancel(errRunStopRequested)
			return true
		}
		return false
	}
	if check() {
		return
	}
	go func() {
		for sleepContext(ctx, runControlInterval) {
			if check() {
				return
			}
		}
	}()
}
