package postgres

import (
	"errors"
	"reflect"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

func TestRunDiagnosticsReplayAndReopen(t *testing.T) {
	ctx, address := newTestSchema(t)
	store, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	token := seedApprovalNotifications(t, ctx, store)
	run := claimTaskControlRun(t, ctx, store, "home", token, "root-run")
	stdout, stderr, exitCode, cache := int64(0), int64(12), uint32(0), int64(23)
	report := &runreport.Diagnostics{StdoutBytes: &stdout, StderrBytes: &stderr, ExitCode: &exitCode,
		Usage: &runreport.Usage{Source: runreport.DSHHeadless, InputTokens: 42, OutputTokens: 5, CachedInputTokens: &cache}}
	if err := recordTaskControlEvent(ctx, store, "home", token, run, 1, workspace.RunEvent{
		Kind: "started", Diagnostics: report,
	}); !errors.Is(err, workspace.ErrInvalidRunDiagnostics) {
		t.Fatalf("未结束轮次接受了诊断: %v", err)
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 1, workspace.RunEvent{Kind: "started", SessionID: "native"}))
	invalid := *report
	invalid.ErrorKind = "raw-error"
	if err := recordTaskControlEvent(ctx, store, "home", token, run, 2, workspace.RunEvent{
		Kind: "completed", Text: "完成", Diagnostics: &invalid,
	}); !errors.Is(err, workspace.ErrInvalidRunDiagnostics) {
		t.Fatalf("接受了非法诊断类别: %v", err)
	}
	terminal := workspace.RunEvent{Kind: "completed", Text: "完成", Diagnostics: report}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 2, terminal))
	before, err := store.Workspace(ctx)
	checkNotification(t, err)
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 2, terminal))
	after, err := store.Workspace(ctx)
	checkNotification(t, err)
	if before.Cursor != after.Cursor {
		t.Fatalf("重放相同诊断推进了游标: %d -> %d", before.Cursor, after.Cursor)
	}
	contradiction := terminal
	contradiction.Diagnostics = &runreport.Diagnostics{ErrorKind: "setup"}
	if err := recordTaskControlEvent(ctx, store, "home", token, run, 2, contradiction); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("接受同一序号下内容不同的诊断: %v", err)
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 3, contradiction))
	store.Close()
	reopened, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(reopened.Close)
	detail, err := reopened.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if len(detail.Runs) != 1 || !reflect.DeepEqual(detail.Runs[0].Diagnostics, report) {
		t.Fatalf("Hub 重启后未保留首个终态的诊断: %+v", detail.Runs)
	}
}

func TestStoppedRunPreservesDiagnostics(t *testing.T) {
	ctx, store, token := openTaskControlFixture(t)
	run := claimTaskControlRun(t, ctx, store, "home", token, "root-run")
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 1, workspace.RunEvent{Kind: "started", SessionID: "native"}))
	controlTaskForTest(t, ctx, store, "root", "stop_current", firstControlRequest, "stopping")
	report := &runreport.Diagnostics{CaptureIncomplete: true, ErrorKind: "interrupted"}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 2, workspace.RunEvent{Kind: "stopped", Diagnostics: report}))
	detail, err := store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if detail.Task.Status != "paused" || len(detail.Runs) != 1 || !reflect.DeepEqual(detail.Runs[0].Diagnostics, report) {
		t.Fatalf("停止时未保存运行诊断: %+v", detail)
	}
}
