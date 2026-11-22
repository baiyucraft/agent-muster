package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/baiyucraft/agent-muster/internal/deviceupdate"
)

// CheckUpdate 上报本机版本及可选的更新进度，并返回当前有效的指派。
// 返回 nil 且无错误表示 Hub 当前没有可领取的更新。
func (client *Client) CheckUpdate(ctx context.Context, identity Identity, check deviceupdate.CheckRequest) (*deviceupdate.Target, error) {
	if err := check.Validate(); err != nil {
		return nil, err
	}
	endpoint, err := client.deviceURL(identity, "update", "check")
	if err != nil {
		return nil, err
	}
	response, err := client.send(ctx, http.MethodPost, endpoint, identity.Token, check, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	const maxResponse = 4096
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	var checkResult deviceupdate.CheckResponse
	if err != nil || len(data) > maxResponse || json.Unmarshal(data, &checkResult) != nil {
		return nil, errors.New("Hub 的更新响应格式无效")
	}
	if checkResult.Target != nil {
		if err := checkResult.Target.Validate(); err != nil {
			return nil, err
		}
	}
	return checkResult.Target, nil
}
