package work

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/gitwork"
)

const gitCandidateMarker = "MUSTER_GIT_CANDIDATE_V1"

const gitFixInstructions = "\n\n请在本轮提供的 Git 工作树内完成修改。.muster-inbox 是设备收件目录，请留在本机。" +
	"设备会在独立检出中测试候选提交，请将推送留给交付流程。"

const gitCandidateInstructions = "\n\n本项目由 daemon 创建候选提交。Agent 只修改项目文件，无需运行 Git 暂存或提交命令。" +
	"普通结果先写业务正文，再另起两行给出提交清单。第一行仅写 " + gitCandidateMarker +
	"。第二行写单行 JSON 对象，例如 " +
	"{\"files\":[\"src/example.go\"],\"message\":\"fix: 修复示例\"}。" +
	"files 填写全部拟交付且已变更的文件，使用工作树根下以 / 分隔的相对路径，删除也填写原文件，重命名填写新旧两个路径。" +
	"最多 256 个文件，暂不支持仅修改大小写的重命名。message 按项目约定编写，最多 4096 个 UTF-8 字节。" +
	"仅需重新验证已创建的候选且未改动文件时，填写空文件列表和空提交说明。" +
	"请示或等待时省略清单。如本轮还要求附上输入回执，把输入回执放在提交清单之后。"

type gitCandidateManifest struct {
	files   []string
	message string
}

// invokeGitFix 在 CLI 退出并撤销本轮协作能力后从成功结果提取清单，再交给 Git 工作树确认。
// 请示、等待和失败只保留原有运行结果。原始 CLI 输出仍可从设备日志核对。
func (worker *Worker) invokeGitFix(ctx context.Context, run control.Run, service *localHelper, checkout gitwork.Workspace, execute agentExecutor, event control.RunEvent, daemonCommit bool) (control.RunEvent, string, error) {
	run.Prompt += gitFixInstructions
	if !daemonCommit {
		run.Prompt += "请逐个暂存和提交修改的项目文件。"
		event, err := worker.invoke(ctx, run, service, checkout.FixDir, execute, event)
		return event, "", err
	}
	head, err := checkout.FixHead(ctx)
	if err != nil {
		return event, "", err
	}
	run.Prompt += gitCandidateInstructions
	event, err = worker.invoke(ctx, run, service, checkout.FixDir, execute, event)
	body, manifest := parseGitCandidate(event.Text)
	event.Text = body
	if err != nil || event.Kind != "completed" {
		return event, "", err
	}
	if manifest == nil || !validInputResultBody(body) {
		return event, "", errors.New("本轮缺少有效的 Git 候选提交清单，已保留修复工作区")
	}
	if len(manifest.files) == 0 {
		// 空清单只能复用本轮启动前已有的候选。验证入口仍须核对工作区干净和提交来源。
		return event, head, nil
	}
	candidate, err := checkout.CommitSelected(ctx, head, manifest.files, manifest.message)
	return event, candidate, err
}

// parseGitCandidate 识别单独一行的清单标记。清单即使格式错误也留在设备日志中，仅上报标记前的业务结果。
func parseGitCandidate(text string) (string, *gitCandidateManifest) {
	for offset := 0; offset < len(text); {
		line, footer, foundNewline := strings.Cut(text[offset:], "\n")
		marker := strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(marker) == gitCandidateMarker {
			body := strings.TrimRight(text[:offset], "\r\n")
			if marker == gitCandidateMarker {
				if manifest := decodeGitCandidate(strings.TrimRight(footer, " \t\r\n")); manifest != nil {
					return body, manifest
				}
			}
			opening := strings.LastIndexByte(body, '\n') + 1
			if strings.HasPrefix(body[opening:], "```") {
				body = strings.TrimRight(body[:opening], "\r\n")
			}
			return body, nil
		}
		if !foundNewline {
			break
		}
		offset += len(line) + 1
	}
	return text, nil
}

// decodeGitCandidate 接受一条有界 JSON 对象，各字段必填且只出现一次。
func decodeGitCandidate(footer string) *gitCandidateManifest {
	if len(footer) > 256<<10 || strings.ContainsAny(footer, "\r\n") || !utf8.ValidString(footer) {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(footer))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	var manifest gitCandidateManifest
	seen := make(map[string]bool, 2)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return nil
		}
		seen[key] = true
		switch key {
		case "files":
			err = decoder.Decode(&manifest.files)
		case "message":
			message, tokenErr := decoder.Token()
			value, ok := message.(string)
			if tokenErr != nil || !ok {
				return nil
			}
			manifest.message = value
		default:
			return nil
		}
		if err != nil {
			return nil
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil
	}
	if _, err := decoder.Token(); err != io.EOF || manifest.files == nil || !seen["message"] {
		return nil
	}
	if len(manifest.files) == 0 && manifest.message != "" || len(manifest.files) != 0 && strings.TrimSpace(manifest.message) == "" {
		return nil
	}
	return &manifest
}
