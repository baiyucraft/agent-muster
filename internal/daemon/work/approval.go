package work

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baiyucraft/agent-muster/internal/approval"
	"github.com/baiyucraft/agent-muster/internal/daemon/control"
)

const approvalMarker = "MUSTER_APPROVAL_V1"

const approvalInstructions = "\n\n只有必须请用户决定且当前无法继续时，才在最终回复中提交请示。" +
	"第一行仅写 " + approvalMarker + "，第二行写单个 JSON 对象，例如 " +
	`{"question":"要用哪种索引格式？","options":["CSV","JSON"],"impact":"决定交付文件格式"}` +
	"。options 和 impact 可省略。问题和影响范围各限 2000 个 UTF-8 字节，最多 8 个选项，每项限 240 字节。" +
	"请示不使用 Markdown 围栏或附带正文，不同时执行本机 muster wait。普通结果仍使用最终文本。"

var errApprovalFormat = errors.New("无法解析本轮请示，已保留原始回复供核对")

// parseApprovalReply 只处理最终回复开头的协议标记。普通文本中的示例不会触发请示。
func parseApprovalReply(text string) (*approval.Question, error) {
	value := strings.TrimSpace(text)
	first, body, _ := strings.Cut(value, "\n")
	first = strings.TrimSpace(first)
	if first != approvalMarker {
		// 把围栏包裹的协议回复视为需要补写的回执。
		if strings.HasPrefix(first, "```") {
			line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
			if strings.TrimSpace(line) == approvalMarker {
				return nil, errApprovalFormat
			}
		}
		return nil, nil
	}
	if len(body) > 64<<10 || !utf8.ValidString(body) {
		return nil, errApprovalFormat
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	var question approval.Question
	if err := decoder.Decode(&question); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errApprovalFormat
	}
	normalized, err := question.Normalize()
	if err != nil {
		return nil, errApprovalFormat
	}
	return &normalized, nil
}

// finishApproval 仅在成功退出后识别请示。需要修正格式时保留原文，并在原会话补写一次。
func (worker *Worker) finishApproval(ctx context.Context, run control.Run, dir string, execute agentExecutor, event *control.RunEvent, waiting bool) error {
	question, err := parseApprovalReply(event.Text)
	if question == nil && err == nil {
		return nil
	}
	if err != nil || event.SessionID == "" || waiting {
		// 原始回执留在本机。上报固定诊断，避免非法字符阻塞补报或泄露子任务正文。
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, saveErr := worker.db.ExecContext(persist, "UPDATE runs SET text=? WHERE id=?", event.Text, run.ID)
		cancel()
		event.Text = ""
		if saveErr != nil {
			event.Kind = "interrupted"
			return errors.Join(err, saveErr)
		}
	}
	if event.SessionID == "" || waiting {
		event.Kind = "interrupted"
		return errors.New("请示需要可续接的原生会话，且不能与依赖等待同时提交")
	}
	if err != nil {
		prompt := "上一轮的请示回执格式无效。只重新输出原问题的结构化请示。不要重做任务、调用工具或修改文件。" + approvalInstructions
		text, retryErr := execute(ctx, run.AgentKind, dir, prompt, event.SessionID, nil, func() error { return nil }, func(id string) error {
			if id != event.SessionID {
				return errors.New("补写请示时会话发生变化")
			}
			return nil
		})
		if retryErr == nil && ctx.Err() == nil {
			question, retryErr = parseApprovalReply(text)
		}
		if question == nil || retryErr != nil || ctx.Err() != nil {
			event.Kind = "interrupted"
			return err
		}
	}
	event.Kind = "approval_requested"
	event.Approval = question
	event.Text = ""
	return nil
}
