package work

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/gitwork"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

func (worker *Worker) invokeGit(ctx context.Context, run control.Run, peer *collaboration, service *localHelper, dir string, execute agentExecutor, event control.RunEvent, readDiagnostics func() *runreport.Diagnostics) (control.RunEvent, error) {
	if !taskStorageID.MatchString(run.TaskID) {
		return event, errors.New("Git 任务标识无效")
	}
	checkout, err := gitwork.Prepare(ctx, dir, filepath.Join(worker.root, "git", run.TaskID), *run.GitPolicy)
	if err != nil {
		return event, err
	}
	var fingerprint string
	if run.GitPublish {
		if _, ok := peerGitClient(peer); !ok {
			return event, errors.New("设备端没有可用的 Git 推送接口")
		}
		fingerprint, err = worker.freezeGitDestination(ctx, run, dir)
		if err != nil {
			return event, err
		}
	}
	acceptance := run.Prompt + inputMessageContext(run.InputMessages)
	daemonCommit := run.GitPolicy.CandidateCommit == gitdelivery.CandidateCommitDaemon
	for repairs := 0; ; repairs++ {
		event.GitVerification = nil
		event.Kind = "failed"
		var confirmedCandidate string
		event, confirmedCandidate, err = worker.invokeGitFix(ctx, run, service, checkout, execute, event, daemonCommit)
		if err != nil || event.Kind != "completed" {
			return event, err
		}
		evidence, feedback, verifyErr := worker.verifyGitCandidate(ctx, checkout, run, confirmedCandidate, acceptance, execute, repairs)
		event.GitVerification = &evidence
		if verifyErr != nil {
			event.Kind = "failed"
			return event, verifyErr
		}
		if feedback == "" {
			if run.GitPublish {
				// 推送待核对时由本机记录恢复终态，先保留当前已确认的修复诊断。
				if run.DiagnosticsSupported {
					if report := readDiagnostics(); report.Valid() {
						event.Diagnostics = report
					}
				}
				return event, worker.publishGitCandidate(ctx, run, peer, dir, checkout, fingerprint, event)
			}
			event.Text = fmt.Sprintf("候选提交 %s 已通过项目验证。\n\n%s", evidence.CandidateOID, event.Text)
			return event, nil
		}
		if repairs >= run.GitPolicy.MaxRepairs || event.SessionID == "" {
			event.Kind = "failed"
			return event, errors.New(feedback)
		}
		run.SessionID = event.SessionID
		// 本轮已经运行过模型。返修遇到的续接错误按普通失败保存。
		run.SessionFailureSupported = false
		run.Prompt = gitRepairPrompt(feedback, evidence, filepath.Join(worker.root, "git", run.TaskID))
		// 输入已在同一运行中处理，返修沿用 event 中的回执和原生会话。
		run.InputIDs = nil
		run.InputMessages = nil
	}
}

func (worker *Worker) verifyGitCandidate(ctx context.Context, checkout gitwork.Workspace, run control.Run, confirmedCandidate, acceptance string, execute agentExecutor, repairs int) (gitdelivery.Verification, string, error) {
	evidence := gitdelivery.Verification{BaseOID: checkout.BaseOID, TestStatus: "unverified", RepairAttempts: repairs}
	candidate, err := checkout.Candidate(ctx)
	if err != nil {
		return evidence, "", err
	}
	if confirmedCandidate != "" && candidate != confirmedCandidate {
		return evidence, "", errors.New("Git 修复工作区的 HEAD 与本轮确认的候选提交不一致")
	}
	evidence.CandidateOID = candidate
	test, err := checkout.Test(ctx, candidate, run.GitPolicy.Test)
	evidence.TestLogRef = test.LogRef
	if err != nil {
		return evidence, "", err
	}
	if test.ExitCode == nil {
		return evidence, "", errors.New("Git 候选测试缺少退出码")
	}
	evidence.TestExitCode = test.ExitCode
	if *test.ExitCode != 0 {
		evidence.TestStatus = "failed"
		return evidence, fmt.Sprintf("候选提交 %s 的项目测试退出码为 %d", candidate, *test.ExitCode), nil
	}
	evidence.TestStatus = "passed"
	if run.GitPolicy.ReviewAgent == "" {
		return evidence, "", nil
	}
	evidence.ReviewAgent = run.GitPolicy.ReviewAgent
	attempt, feedback, err := worker.runGitReview(ctx, checkout, run, acceptance, execute, runreport.ReviewAttempt{
		CandidateOID: candidate, RepairIndex: repairs, AgentKind: evidence.ReviewAgent, Status: "unverified",
	})
	evidence.ReviewStatus = attempt.Status
	return evidence, feedback, err
}

// runGitReview 为候选创建独立 CLI 诊断。仅在 Git 工作树和候选重核通过后确认审查结论。
func (worker *Worker) runGitReview(ctx context.Context, checkout gitwork.Workspace, run control.Run, acceptance string, execute agentExecutor, attempt runreport.ReviewAttempt) (runreport.ReviewAttempt, string, error) {
	reviewCtx, invocation, readDiagnostics := runner.WithReviewDiagnostics(ctx, worker.runLogDirectory(run.ID))
	if run.ReviewDiagnosticsSupported {
		attempt.LogInvocation = invocation
		attempt.Diagnostics = &runreport.Diagnostics{ErrorKind: "interrupted", CaptureIncomplete: true}
		if err := worker.saveGitReview(ctx, run.ID, attempt); err != nil {
			return attempt, "", err
		}
	}
	var invocationErr error
	var reviewStatus, feedback string
	err := checkout.Review(reviewCtx, attempt.CandidateOID, func(ctx context.Context, dir, base, sha string) error {
		prompt := fmt.Sprintf("独立审查候选提交 %s 相对基线 %s 的改动。验收要求如下。检查相关差异与风险，保持审查工作树原样。首行只使用 MUSTER_REVIEW_PASS 或 MUSTER_REVIEW_CHANGES，后者在后续行写明须修的问题。\n\n%s", sha, base, acceptance)
		var output string
		output, invocationErr = execute(ctx, attempt.AgentKind, dir, prompt, "", nil, func() error { return nil }, func(string) error { return nil })
		if invocationErr != nil {
			return invocationErr
		}
		line, note, _ := strings.Cut(strings.TrimSpace(output), "\n")
		switch strings.TrimSpace(line) {
		case "MUSTER_REVIEW_PASS":
			reviewStatus = "passed"
		case "MUSTER_REVIEW_CHANGES":
			if strings.TrimSpace(note) == "" {
				return errors.New("独立 Review 未说明需修正的问题")
			}
			reviewStatus, feedback = "changes_requested", trimUTF8(note, 4000)
		default:
			return errors.New("独立 Review 未提供可核对的结论")
		}
		return nil
	})
	if err == nil {
		attempt.Status = reviewStatus
	} else {
		feedback = ""
	}
	if run.ReviewDiagnosticsSupported {
		attempt.Diagnostics = readDiagnostics()
		if attempt.Diagnostics == nil && invocationErr != nil {
			attempt.Diagnostics = &runreport.Diagnostics{ErrorKind: "setup"}
		}
		if attempt.Diagnostics != nil && attempt.Diagnostics.ErrorKind == "" && invocationErr != nil {
			attempt.Diagnostics.ErrorKind = "output"
		}
		if attempt.Diagnostics == nil || attempt.Diagnostics.StdoutBytes == nil && attempt.Diagnostics.StderrBytes == nil {
			attempt.LogInvocation = ""
		}
		if !attempt.Diagnostics.Valid() {
			attempt.Diagnostics = nil
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if saveErr := worker.saveGitReview(persist, run.ID, attempt); saveErr != nil {
			attempt.Status = "unverified"
			return attempt, "", errors.Join(err, saveErr)
		}
	}
	return attempt, feedback, err
}

func gitRepairPrompt(feedback string, evidence gitdelivery.Verification, dir string) string {
	prompt := "验证发现以下问题。请在当前修复工作树内处理，保留设备收件目录。修改后的候选将重新测试和审查。\n\n" + feedback
	if evidence.TestLogRef != "" {
		log, err := os.Open(filepath.Join(dir, filepath.FromSlash(evidence.TestLogRef)))
		if err == nil {
			defer log.Close()
			buf := make([]byte, 4096)
			n, _ := log.Read(buf)
			if n > 0 {
				prompt += "\n\n项目测试日志片段：\n" + trimUTF8(string(buf[:n]), 4096)
			}
		}
	}
	return prompt
}
