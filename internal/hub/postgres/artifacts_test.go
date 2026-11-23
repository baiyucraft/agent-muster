package postgres

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/baiyucraft/agent-muster/internal/artifact"
	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func TestRunArtifactEvidenceAndReport(t *testing.T) {
	ctx, store, token := openTaskControlFixture(t)
	run := claimTaskControlRun(t, ctx, store, "home", token, "root-run")
	if !run.ArtifactsSupported {
		t.Fatal("普通目录领取未声明支持产物")
	}
	record := func(sequence int64, event workspace.RunEvent) error {
		return recordTaskControlEvent(ctx, store, "home", token, run, sequence, event)
	}
	ref := makeTestArtifact(run.ID, "成果/索引.csv")
	if err := record(1, workspace.RunEvent{Kind: "started", Artifacts: []artifact.Reference{ref}}); !errors.Is(err, workspace.ErrInvalidArtifacts) {
		t.Fatalf("启动事件错误接受产物: %v", err)
	}
	checkNotification(t, record(1, workspace.RunEvent{Kind: "session", SessionID: "native"}))
	if err := record(2, workspace.RunEvent{Kind: "completed", Text: "尚未启动", Artifacts: []artifact.Reference{ref}}); !errors.Is(err, workspace.ErrInvalidArtifacts) {
		t.Fatalf("仅有会话事件的轮次错误接受产物: %v", err)
	}
	checkNotification(t, record(2, workspace.RunEvent{Kind: "started", SessionID: "native"}))
	wrongRun := makeTestArtifact("different-run", ref.Path)
	if err := record(3, workspace.RunEvent{Kind: "completed", Text: "证据", Artifacts: []artifact.Reference{wrongRun}}); !errors.Is(err, workspace.ErrInvalidArtifacts) {
		t.Fatalf("错误轮次的产物被保存: %v", err)
	}
	event := workspace.RunEvent{Kind: "completed", Text: strings.Repeat("结", 4000), Artifacts: []artifact.Reference{ref}}
	checkNotification(t, record(3, event))
	checkNotification(t, record(3, event))
	badSize := ref
	badSize.Size++
	if err := record(4, workspace.RunEvent{Kind: "completed", Text: "冲突", Artifacts: []artifact.Reference{badSize}}); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("相同 ID 接受不同大小: %v", err)
	}
	late := makeTestArtifact(run.ID, "成果/补记.csv")
	lateRefs := []artifact.Reference{ref, late}
	for i := len(lateRefs); i < artifact.MaxCount; i++ {
		lateRefs = append(lateRefs, makeTestArtifact(run.ID, fmt.Sprintf("成果/补记-%d.csv", i)))
	}
	lateEvent := workspace.RunEvent{Kind: "completed", Text: "迟到事件", Artifacts: lateRefs}
	checkNotification(t, record(4, lateEvent))
	checkNotification(t, record(4, lateEvent))
	overflow := makeTestArtifact(run.ID, "成果/超限.csv")
	if err := record(5, workspace.RunEvent{Kind: "completed", Text: "超限", Artifacts: []artifact.Reference{overflow}}); !errors.Is(err, workspace.ErrInvalidArtifacts) {
		t.Fatalf("累计超限的产物被保存: %v", err)
	}
	checkNotification(t, record(5, workspace.RunEvent{Kind: "completed", Text: "仍可重复引用", Artifacts: []artifact.Reference{late}}))

	item, err := store.Artifact(ctx, run.ID, ref.ID)
	checkNotification(t, err)
	if item.Reference != ref || item.DeviceID != "home" || item.RunID != run.ID || item.TaskID != "root" || item.CreatedAt.IsZero() {
		t.Fatalf("产物引用缺少来源或校验数据: %+v", item)
	}
	if _, err := store.Artifact(ctx, "different-run", ref.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("跨轮次查找返回 %v", err)
	}
	detail, err := store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if len(detail.Artifacts) != artifact.MaxCount || detail.Task.Result != event.Text || detail.Task.Status != "completed" {
		t.Fatalf("重复或迟到回执改变了终态或产物数量: %+v", detail.Task)
	}
	report, err := store.ClaimDelivery(ctx)
	checkNotification(t, err)
	if report.TaskID != "root" || len(report.Text) > 12000 || !utf8.ValidString(report.Text) ||
		!strings.Contains(report.Text, "home · "+ref.Path) || !strings.Contains(report.Text, ref.SHA256) || strings.Contains(report.Text, late.Path) {
		t.Fatalf("汇报未优先保留有效产物或长度溢出: %d 字节", len(report.Text))
	}
}

func TestLateArtifactRequiresPriorLaunch(t *testing.T) {
	ctx, store, token := openTaskControlFixture(t)
	run := claimTaskControlRun(t, ctx, store, "home", token, "root-run")
	record := func(sequence int64, event workspace.RunEvent) error {
		return recordTaskControlEvent(ctx, store, "home", token, run, sequence, event)
	}
	checkNotification(t, record(1, workspace.RunEvent{Kind: "failed", Text: "未启动"}))
	ref := makeTestArtifact(run.ID, "未启动.csv")
	late := workspace.RunEvent{Kind: "failed", Text: "迟到事件", Artifacts: []artifact.Reference{ref}}
	if err := record(2, late); !errors.Is(err, workspace.ErrInvalidArtifacts) {
		t.Fatalf("未启动轮次的迟到回执错误接受产物: %v", err)
	}
	checkNotification(t, record(2, workspace.RunEvent{Kind: "started"}))
	if err := record(3, late); !errors.Is(err, workspace.ErrInvalidArtifacts) {
		t.Fatalf("迟到启动事件错误授权产物: %v", err)
	}
}

func makeTestArtifact(runID, path string) artifact.Reference {
	digest := strings.Repeat("a", 64)
	return artifact.Reference{ID: artifact.ID(runID, path, digest), Path: path, SHA256: digest, Size: 9}
}
