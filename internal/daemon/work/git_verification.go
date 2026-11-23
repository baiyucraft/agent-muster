package work

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/gitwork"
	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
)

func (worker *Worker) invokeGit(ctx context.Context, run control.Run, service *localHelper, dir string, execute agentExecutor, event control.RunEvent) (control.RunEvent, error) {
	if !taskStorageID.MatchString(run.TaskID) {
		return event, errors.New("Git 任务标识无效")
	}
	checkout, err := gitwork.Prepare(ctx, dir, filepath.Join(worker.root, "git", run.TaskID), *run.GitPolicy)
	if err != nil {
		return event, err
	}
	run.Prompt += "\n\n请在本轮提供的 Git 工作树内完成修改，并逐个暂存和提交修改的项目文件。.muster-inbox 是设备收件目录，请留在本机。设备会在独立检出中测试候选提交，请将推送留给交付流程。"
	event, err = worker.invoke(ctx, run, service, checkout.FixDir, execute, event)
	if err != nil || event.Kind != "completed" {
		return event, err
	}
	evidence := &gitdelivery.Verification{BaseOID: checkout.BaseOID, TestStatus: "unverified"}
	event.GitVerification = evidence
	evidence.CandidateOID, err = checkout.Candidate(ctx)
	if err != nil {
		event.Kind = "failed"
		return event, err
	}
	test, err := checkout.Test(ctx, evidence.CandidateOID, run.GitPolicy.Test)
	evidence.TestLogRef = test.LogRef
	if err != nil {
		event.Kind = "failed"
		return event, err
	}
	evidence.TestExitCode = test.ExitCode
	if test.ExitCode == nil {
		event.Kind = "failed"
		return event, errors.New("Git 候选测试缺少退出码")
	}
	if *test.ExitCode != 0 {
		event.Kind = "failed"
		evidence.TestStatus = "failed"
		return event, fmt.Errorf("候选提交 %s 的项目测试退出码为 %d", evidence.CandidateOID, *test.ExitCode)
	}
	evidence.TestStatus = "passed"
	event.Text = fmt.Sprintf("候选提交 %s 已通过项目测试。\n\n%s", evidence.CandidateOID, event.Text)
	return event, nil
}
