package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/runreport"
)

func TestReviewDiagnosticsUseTheirOwnDurableInvocation(t *testing.T) {
	root := t.TempDir()
	ctx, readRepair := WithDiagnostics(context.Background(), root)
	repair := diagnostics(ctx)
	stdout, stderr := repair.openStreams()
	repair.finishStreams(stdout, stderr, false)
	repair.report.Usage = &runreport.Usage{Source: runreport.CodexExec, InputTokens: 11, OutputTokens: 3}
	repair.report.ExitCode = uint32Pointer(0)
	if got := readRepair(); got == nil || got.Usage.InputTokens != 11 {
		t.Fatalf("修复诊断初始化失败：%+v", got)
	}

	reviewCtx, invocation, readReview := WithReviewDiagnostics(ctx, root)
	if !runreport.ValidReviewLogInvocation(invocation) {
		t.Fatalf("Review 日志目录不符合契约：%q", invocation)
	}
	review := diagnostics(reviewCtx)
	stdout, stderr = review.openStreams()
	_, _ = stdout.Write([]byte("review output"))
	_, _ = stderr.Write([]byte("review error"))
	review.finishStreams(stdout, stderr, false)
	got := readReview()
	if got == nil || got.StdoutBytes == nil || *got.StdoutBytes != int64(len("review output")) ||
		got.StderrBytes == nil || *got.StderrBytes != int64(len("review error")) {
		t.Fatalf("Review CLI 诊断不完整：%+v", got)
	}
	if _, err := os.Stat(filepath.Join(root, invocation, "stdout.log")); err != nil {
		t.Fatalf("Review 输出未保留：%v", err)
	}
	if got := readRepair(); got == nil || got.Usage.InputTokens != 11 || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("Review 重置或覆盖了修复诊断：%+v", got)
	}

	ctx, _ = WithDiagnostics(context.Background(), root)
	next := diagnostics(ctx)
	out, errOut := next.openStreams()
	next.finishStreams(out, errOut, false)
	out, errOut = next.openStreams()
	next.finishStreams(out, errOut, false)
	if _, err := os.Stat(filepath.Join(root, "cli-2", "stdout.log")); err != nil {
		t.Fatalf("CLI 日志未采用契约目录格式：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "cli-3", "stdout.log")); err != nil {
		t.Fatalf("重试 CLI 日志未使用新的调用目录：%v", err)
	}
}

func uint32Pointer(value uint32) *uint32 { return &value }
