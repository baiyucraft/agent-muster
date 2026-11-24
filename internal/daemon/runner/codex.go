package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/runreport"
)

var codexThreadID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// RunCodex 在绑定目录通过标准输入向 Codex 传递提示词。续接时校验返回的原生线程，
// 并在解析到 thread.started 后通过 session 保存线程引用。
// 退出码为 0、收到 turn.completed、未收到 turn.failed 且取得最终回复后才返回成功。
func RunCodex(ctx context.Context, directory, prompt, threadID string, env []string, started func() error, session func(string) error) (string, error) {
	if threadID != "" && !codexThreadID.MatchString(threadID) {
		return "", errors.New("无效的 Codex 会话 ID")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		return "", errors.New("无法启动 Codex，请检查本机安装和 PATH")
	}
	output, err := os.CreateTemp("", "agent-muster-codex-final-*.txt")
	if err != nil {
		return "", err
	}
	outputPath := output.Name()
	if err := output.Close(); err != nil {
		os.Remove(outputPath)
		return "", err
	}
	defer os.Remove(outputPath)

	args := []string{"exec"}
	if threadID != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--skip-git-repo-check", "--output-last-message", outputPath)
	if threadID != "" {
		args = append(args, threadID)
	}
	args = append(args, "-")
	cmd, err := probeCommand(ctx, path, args...)
	if err != nil {
		return "", err
	}
	cmd.Dir = directory
	cmd.Env = filteredAgentEnvironment(cmd.Environ(), env)
	var result codexResult
	var resumeStderr boundedOutput
	var stderr io.Writer
	if threadID != "" {
		stderr = &resumeStderr
	}
	exitCode, err := runInJob(ctx, cmd, prompt, started, func(stdout io.Reader) error {
		var parseErr error
		result, parseErr = readCodex(stdout, threadID, session)
		return parseErr
	}, stderr)
	final, outputErr := readFinalMessage(outputPath)
	if final == "" {
		final = result.Final
	}
	if err != nil {
		return final, err
	}
	if outputErr == nil && final == "" && !result.sawStdout && codexResumeUnavailable(threadID, exitCode, string(resumeStderr.data)) {
		return "", fmt.Errorf("Codex 未找到原生会话: %w", ErrSessionUnavailable)
	}
	if err := result.exitError(exitCode); err != nil {
		return final, err
	}
	if final == "" || outputErr != nil {
		if outputErr != nil {
			return final, fmt.Errorf("读取 Codex 最终回复失败: %w", outputErr)
		}
		return final, errors.New("Codex 没有提供最终回复")
	}
	recordUsage(ctx, result.Usage)
	return final, nil
}

type codexResult struct {
	SessionID   string
	Final       string
	Completed   bool
	Failure     string
	TurnFailure string
	Usage       *runreport.Usage
	sawStdout   bool
	turnStarted bool
}

// codexResumeUnavailable 只接受 Codex 0.162.1 在采纳指定线程前输出的单行拒绝。
func codexResumeUnavailable(threadID string, exitCode uint32, stderr string) bool {
	const prefix = "Error: thread/resume: thread/resume failed: no rollout found for thread id "
	const suffix = " (code -32600)"
	line := strings.TrimSuffix(strings.TrimSuffix(stderr, "\n"), "\r")
	return threadID != "" && exitCode == 1 && line == prefix+threadID+suffix
}

// exitError 在确认真实进程退出码后判断完整回合，避免把线程拒绝误作输出读取中断。
func (result codexResult) exitError(exitCode uint32) error {
	switch {
	case result.SessionID == "":
		return errors.New("Codex 没有提供 thread.started")
	case !result.turnStarted:
		return errors.New("Codex 没有提供 turn.started")
	case result.TurnFailure != "":
		return errors.New(result.TurnFailure)
	case exitCode != 0 && result.Failure != "":
		return errors.New(result.Failure)
	case exitCode != 0:
		return fmt.Errorf("Codex 退出码 %d", exitCode)
	case !result.Completed:
		return errors.New("Codex 没有返回 turn.completed")
	default:
		return nil
	}
}

func readCodex(stdout io.Reader, expectedThreadID string, persist func(string) error) (codexResult, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 8192), 4<<20)
	var result codexResult
	for scanner.Scan() {
		result.sawStdout = true
		var event struct {
			Type     string          `json:"type"`
			ThreadID string          `json:"thread_id"`
			Message  string          `json:"message"`
			Usage    json.RawMessage `json:"usage"`
			Error    struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		switch event.Type {
		case "thread.started":
			if !codexThreadID.MatchString(event.ThreadID) || expectedThreadID != "" && event.ThreadID != expectedThreadID {
				return result, errors.New("Codex 返回了不匹配的 thread ID")
			}
			if result.SessionID != "" && result.SessionID != event.ThreadID {
				return result, errors.New("Codex 在同一轮发出了多个 thread ID")
			}
			if result.SessionID == "" {
				if err := persist(event.ThreadID); err != nil {
					return result, fmt.Errorf("无法保存 Codex thread ID: %w", err)
				}
				result.SessionID = event.ThreadID
			}
		case "turn.started":
			result.turnStarted = true
		case "item.completed":
			if event.Item.Type == "agent_message" {
				result.Final = event.Item.Text
			}
		case "turn.completed":
			if result.TurnFailure != "" {
				continue
			}
			if !result.Completed && result.SessionID != "" && result.turnStarted {
				result.Usage = codexUsage(event.Usage, expectedThreadID != "")
			} else {
				result.Usage = nil
			}
			result.Completed = true
		case "turn.failed":
			result.Completed = false
			result.Usage = nil
			if result.TurnFailure == "" {
				result.TurnFailure = event.Error.Message
				if result.TurnFailure == "" {
					result.TurnFailure = "Codex turn.failed"
				}
			}
		case "error":
			if event.Message != "" {
				result.Failure = event.Message
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("无法读取 Codex JSONL: %w", err)
	}
	return result, nil
}

func readFinalMessage(path string) (string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	const limit = 2 << 20
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if len(data) > limit {
		return "", errors.New("最终回复超过 2 MiB 限制")
	}
	return strings.TrimSpace(string(data)), nil
}

func filteredAgentEnvironment(environment, additions []string) []string {
	result := make([]string, 0, len(environment)+len(additions))
	for _, item := range environment {
		name, _, _ := strings.Cut(item, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "MUSTER_") {
			result = append(result, item)
		}
	}
	for _, item := range additions {
		name, _, _ := strings.Cut(item, "=")
		if name == "MUSTER_PIPE" || name == "MUSTER_RUN_CAPABILITY" {
			result = append(result, item)
		}
	}
	return result
}
