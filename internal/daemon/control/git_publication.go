package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
)

// AuthorizeGitPush 提交候选验证证据和设备解析的端点指纹，并取得当前运行的推送意图。
// 调用方须核对冻结目标和 PushAllowed 后再推送，重试时复用同一证据和端点指纹。
func (client *Client) AuthorizeGitPush(ctx context.Context, identity Identity, runID string, input gitdelivery.PushRequest) (gitdelivery.PushIntent, error) {
	intent, err := client.requestGitPush(ctx, identity, http.MethodPost, input, "runs", runID, "git-push")
	if err == nil && (intent.CandidateOID != input.Verification.CandidateOID || intent.DestinationFingerprint != input.DestinationFingerprint) {
		return gitdelivery.PushIntent{}, errors.New("Hub 的推送意图与候选或端点指纹不一致")
	}
	return intent, err
}

// ObserveGitPush 上报指定推送意图的远端分支读回。它返回 Hub 已记录的状态。
func (client *Client) ObserveGitPush(ctx context.Context, identity Identity, runID, intentID string, input gitdelivery.Observation) (gitdelivery.PushIntent, error) {
	intent, err := client.requestGitPush(ctx, identity, http.MethodPut, input, "runs", runID, "git-push", intentID, "observation")
	if err == nil && (intent.ID != intentID || intent.PushAllowed) {
		return gitdelivery.PushIntent{}, errors.New("Hub 返回了无效的 Git 读回确认")
	}
	return intent, err
}

func (client *Client) requestGitPush(ctx context.Context, identity Identity, method string, input any, path ...string) (gitdelivery.PushIntent, error) {
	var intent gitdelivery.PushIntent
	endpoint, err := client.deviceURL(identity, path...)
	if err != nil {
		return intent, err
	}
	response, err := client.send(ctx, method, endpoint, identity.Token, input, http.StatusOK)
	if err != nil {
		return intent, err
	}
	defer response.Body.Close()
	const limit = 4096
	payload, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(payload) > limit || json.Unmarshal(payload, &intent) != nil ||
		!strings.HasPrefix(intent.ID, "push-") || intent.Revision <= 0 || intent.Remote == "" ||
		!strings.HasPrefix(intent.TargetRef, "refs/heads/") || intent.CandidateOID == "" ||
		len(intent.DestinationFingerprint) != 64 {
		return gitdelivery.PushIntent{}, errors.New("Hub 的 Git 推送响应格式无效")
	}
	switch intent.Status {
	case "pending", "missing", "unavailable", "diverged", "same_commit", "contains_commit":
		if intent.Status == "pending" || !intent.PushAllowed {
			return intent, nil
		}
	}
	return gitdelivery.PushIntent{}, errors.New("Hub 返回了无效的 Git 推送状态")
}
