package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/baiyucraft/agent-muster/internal/runreport"
)

// RunLogRequests 领取已分配给当前设备的临时日志读取请求。旧版 Hub 返回 404 时调用方可继续执行任务。
func (client *Client) RunLogRequests(ctx context.Context, identity Identity) ([]runreport.LogRequest, error) {
	endpoint, err := client.deviceURL(identity, "run-log-requests")
	if err != nil {
		return nil, err
	}
	response, err := client.send(ctx, http.MethodGet, endpoint, identity.Token, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	const limit = 4096
	payload, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	var result struct {
		Requests []runreport.LogRequest `json:"requests"`
	}
	if len(payload) > limit || json.Unmarshal(payload, &result) != nil ||
		result.Requests == nil || len(result.Requests) > runreport.MaxLogRequests {
		return nil, errors.New("Hub 的日志请求清单格式无效")
	}
	for _, request := range result.Requests {
		if err := request.Validate(); err != nil {
			return nil, err
		}
	}
	return result.Requests, nil
}

// ReportRunLogs 把一次读取结果提交给等待该请求的 Hub。请求已经过期时，Hub 返回 404。
func (client *Client) ReportRunLogs(ctx context.Context, identity Identity, request runreport.LogRequest, logs runreport.LogResponse) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := logs.Validate(); err != nil {
		return err
	}
	endpoint, err := client.deviceURL(identity, "run-log-requests", request.ID)
	if err != nil {
		return err
	}
	response, err := client.send(ctx, http.MethodPut, endpoint, identity.Token, logs, http.StatusNoContent)
	if err == nil {
		response.Body.Close()
	}
	return err
}
