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
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
	"github.com/baiyucraft/agent-muster/internal/inputreceipt"
)

const inputReceiptMarker = "MUSTER_INPUT_RECEIPT_V1"

var errInputReceipt = errors.New("本轮输入回执无效或不完整，已在设备保留原始回复供核对")

// inputMessageContext 按领取快照提供以前轮次仍待确认的用户要求。
func inputMessageContext(messages map[string]string) string {
	if len(messages) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(messages)
	return "\n\n以下是此前提供且仍待确认的用户消息。先核对已有文件与改动，保留已完成的工作，再处理余项。\n" + string(encoded)
}

// inputReceiptPrompt 使用领取时冻结的输入 ID。补写时原始输入留在同一会话中。
func inputReceiptPrompt(ids []string) string {
	encoded, _ := json.Marshal(ids)
	return "\n\n本轮可确认的输入 ID 为 " + string(encoded) + "。以 transfer: 开头的 ID 对应附件列表中同名投递。" +
		"只为实际处理的输入提交回执。正常完成须确认全部输入。请示或等待时可以确认部分输入，无已处理项时写空数组。" +
		"普通结果和结构化请示都在末尾另起两行附上回执。如果本次只补写回执，直接输出这两行。" +
		"第一行仅写 " + inputReceiptMarker + "，第二行是单行 JSON 对象，唯一字段为 handled_input_ids。" +
		"回执使用原始 ID，输出后结束回复。"
}

// parseInputReceipt 读取最终独立标记之后的单行 JSON。格式或 ID 无效时仍提取标记前的业务正文。
func parseInputReceipt(text string, expected []string) (string, *inputreceipt.Receipt) {
	end := strings.TrimRight(text, " \t\r\n")
	start := len(end)
	for start > 0 {
		line := strings.LastIndexByte(end[:start], '\n') + 1
		marker := strings.TrimSuffix(end[line:start], "\r")
		if strings.TrimSpace(marker) == inputReceiptMarker {
			body := strings.TrimRight(end[:line], "\r\n")
			footer, _ := strings.CutPrefix(end[start:], "\n")
			if marker == inputReceiptMarker {
				if receipt := decodeInputReceipt(footer, expected); receipt != nil {
					return body, receipt
				}
			}
			// 独立标记之后属于协议，格式不完整时也不把残缺字段写进业务结果。
			opening := strings.LastIndexByte(body, '\n') + 1
			if strings.HasPrefix(body[opening:], "```") && strings.HasSuffix(footer, "\n```") {
				body = strings.TrimRight(body[:opening], "\r\n")
			}
			return body, nil
		}
		start = line - 1
	}
	return text, nil
}

// decodeInputReceipt 只接受一个大小有界的单行对象。精确读取键名以拒绝重复键、未知键和 null。
func decodeInputReceipt(footer string, expected []string) *inputreceipt.Receipt {
	if len(footer) > 64<<10 || strings.ContainsAny(footer, "\r\n") || !utf8.ValidString(footer) {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(footer))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	if key, err := decoder.Token(); err != nil || key != "handled_input_ids" {
		return nil
	}
	var receipt inputreceipt.Receipt
	if err := decoder.Decode(&receipt.HandledIDs); err != nil || !receipt.ValidFor(expected, false) {
		return nil
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil
	}
	return &receipt
}

// saveInputReceiptOriginal 保存补写前的回复。独立记录避免后续终态、Git 返修或重启恢复覆盖业务原文。
func (worker *Worker) saveInputReceiptOriginal(ctx context.Context, runID, text string) error {
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := worker.db.ExecContext(persist, `INSERT INTO input_receipt_original_replies(run_id,body)
		VALUES (?,?) ON CONFLICT(run_id) DO NOTHING`, runID, []byte(text))
	return err
}

// finishWithInputReceipt 校验输入确认和请示。两个协议共用一次原生会话补写，业务正文始终取自原始回复。
func (worker *Worker) finishWithInputReceipt(ctx context.Context, run control.Run, dir string, execute agentExecutor, event *control.RunEvent, waiting bool) error {
	original := event.Text
	body, receipt := parseInputReceipt(original, run.InputIDs)
	var question *approval.Question
	var approvalErr error
	if run.ApprovalSupported {
		question, approvalErr = parseApprovalReply(body)
	}
	event.InputReceipt = receipt
	invalidApproval := (question != nil || approvalErr != nil) && (waiting || event.SessionID == "")
	ordinaryResult := question == nil && approvalErr == nil && !waiting
	invalidBody := ordinaryResult && !validInputResultBody(body)
	repairReceipt := receipt == nil || ordinaryResult && !receipt.ValidFor(run.InputIDs, true)
	if repairReceipt || approvalErr != nil || invalidApproval || invalidBody {
		event.Kind, event.Text = "interrupted", ""
		if err := worker.saveInputReceiptOriginal(ctx, run.ID, original); err != nil {
			return errors.Join(errInputReceipt, err)
		}
		if invalidApproval || invalidBody || event.SessionID == "" || ctx.Err() != nil {
			return errInputReceipt
		}
		var err error
		receipt, question, err = repairInputReceipt(ctx, run, dir, execute, event.SessionID, receipt, question, repairReceipt, approvalErr != nil)
		event.InputReceipt = receipt
		if err != nil {
			return err
		}
		if question == nil && !waiting && !receipt.ValidFor(run.InputIDs, true) {
			return errInputReceipt
		}
	}
	event.Kind, event.Text = "completed", body
	if question != nil {
		event.Kind, event.Text, event.Approval = "approval_requested", "", question
	}
	return nil
}

// validInputResultBody 检查普通完成的原始业务正文。补写不能提供替代业务结果。
func validInputResultBody(body string) bool {
	return strings.TrimSpace(body) != "" && utf8.ValidString(body) && !strings.ContainsRune(body, 0)
}

// repairInputReceipt 只请求缺损的协议部分。启动时沿用原会话且不授予本机协作能力。
func repairInputReceipt(ctx context.Context, run control.Run, dir string, execute agentExecutor, sessionID string, receipt *inputreceipt.Receipt, question *approval.Question, repairReceipt, repairApproval bool) (*inputreceipt.Receipt, *approval.Question, error) {
	prompt := "上一回复的结构化回执格式无效。仅补写下面要求的回执。保持原业务结果，禁止重做任务、调用工具或修改文件。"
	if repairApproval {
		prompt += "只重新输出原问题的结构化请示。" + approvalInstructions
	} else {
		prompt += "本次不附带业务正文或请示。"
	}
	if repairReceipt {
		prompt += inputReceiptPrompt(run.InputIDs)
	}
	text, err := execute(ctx, run.AgentKind, dir, prompt, sessionID, nil, func() error { return nil }, func(id string) error {
		if id != sessionID {
			return errors.New("补写输入回执时会话发生变化")
		}
		return nil
	})
	if errors.Is(err, runner.ErrProcessExitUnconfirmed) {
		return receipt, question, err
	}
	if err != nil || ctx.Err() != nil {
		return receipt, question, errInputReceipt
	}
	if repairReceipt {
		var refreshed *inputreceipt.Receipt
		text, refreshed = parseInputReceipt(text, run.InputIDs)
		if refreshed == nil || receipt != nil && !receipt.ValidFor(refreshed.HandledIDs, false) {
			return receipt, question, errInputReceipt
		}
		if !repairApproval && strings.TrimSpace(text) != "" {
			return receipt, question, errInputReceipt
		}
		receipt = refreshed
	}
	if repairApproval {
		question, err = parseApprovalReply(text)
		if err != nil || question == nil {
			return receipt, nil, errInputReceipt
		}
	}
	return receipt, question, nil
}
