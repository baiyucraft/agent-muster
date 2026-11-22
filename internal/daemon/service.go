// Package daemon 管理已配对设备向 Hub 上报心跳和 Agent 清单的循环。
package daemon

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

type reporter interface {
	Heartbeat(context.Context, control.Identity) error
	ReportInventory(context.Context, control.Identity, []runner.Agent) error
}

// Event 记录心跳或清单上报观察到的设备状态变化。
type Event struct {
	Kind   string
	Source string
	At     time.Time
	Agents []runner.Agent
	Err    error
}

const (
	EventHeartbeat = "heartbeat"
	EventInventory = "inventory"
	EventError     = "error"
	EventRevoked   = "revoked"
)

// Run 独立运行设备心跳与本机 Agent 探测，以避免探测阻塞心跳。
func Run(ctx context.Context, client *control.Client, identity control.Identity) error {
	return RunWithObserver(ctx, client, identity, func(event Event) {
		if event.Kind == EventError {
			log.Printf("设备 %s 上报失败: %v", event.Source, event.Err)
		}
	})
}

// RunWithObserver 分别运行设备心跳和清单上报，并串行回调 observer。
// 回调只接收状态事件，设备身份凭据由上报循环持有。
func RunWithObserver(ctx context.Context, client *control.Client, identity control.Identity, observer func(Event)) error {
	var observerLock sync.Mutex
	serializedObserver := func(event Event) {
		if observer == nil {
			return
		}
		observerLock.Lock()
		defer observerLock.Unlock()
		observer(event)
	}
	return run(ctx, client, identity, runner.Discover, serializedObserver)
}

func run(ctx context.Context, client reporter, identity control.Identity, discover func(context.Context) []runner.Agent, observer func(Event)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errorsFromLoops := make(chan error, 2)
	go heartbeatLoop(ctx, client, identity, errorsFromLoops, observer)
	go inventoryLoop(ctx, client, identity, discover, errorsFromLoops, observer)

	select {
	case <-ctx.Done():
		return nil
	case err := <-errorsFromLoops:
		cancel()
		return err
	}
}

func heartbeatLoop(ctx context.Context, client reporter, identity control.Identity, fatal chan<- error, observer func(Event)) {
	ticker := time.NewTicker(control.HeartbeatInterval)
	defer ticker.Stop()
	for {
		if err := client.Heartbeat(ctx, identity); err != nil {
			if errors.Is(err, control.ErrUnauthorized) {
				notify(observer, Event{Kind: EventRevoked, Source: "heartbeat", At: time.Now(), Err: err})
				fatal <- err
				return
			}
			notify(observer, Event{Kind: EventError, Source: "heartbeat", At: time.Now(), Err: err})
		} else {
			notify(observer, Event{Kind: EventHeartbeat, At: time.Now()})
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func inventoryLoop(ctx context.Context, client reporter, identity control.Identity, discover func(context.Context) []runner.Agent, fatal chan<- error, observer func(Event)) {
	ticker := time.NewTicker(control.InventoryInterval)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		agents := discover(probeCtx)
		cancel()
		if err := client.ReportInventory(ctx, identity, agents); err != nil {
			if errors.Is(err, control.ErrUnauthorized) {
				notify(observer, Event{Kind: EventRevoked, Source: "inventory", At: time.Now(), Err: err})
				fatal <- err
				return
			}
			notify(observer, Event{Kind: EventError, Source: "inventory", At: time.Now(), Err: err})
		} else {
			notify(observer, Event{Kind: EventInventory, At: time.Now(), Agents: agents})
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func notify(observer func(Event), event Event) {
	if observer != nil {
		observer(event)
	}
}
