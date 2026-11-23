package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/baiyucraft/agent-muster/internal/artifact"
)

var artifactRunPattern = regexp.MustCompile(`^run-[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// ArtifactRead 获取本设备待处理的下载请求。Hub 暂无请求时返回零值。
func (client *Client) ArtifactRead(ctx context.Context, identity Identity) (artifact.ReadRequest, error) {
	var request artifact.ReadRequest
	endpoint, err := client.deviceURL(identity, "artifact-reads")
	if err != nil {
		return request, err
	}
	response, err := client.send(ctx, http.MethodGet, endpoint, identity.Token, nil, http.StatusOK, http.StatusNoContent)
	if err != nil {
		return request, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return request, nil
	}
	const limit = 1024
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return request, err
	}
	if len(body) > limit || json.Unmarshal(body, &request) != nil || !artifact.ValidReadID(request.ID) ||
		!artifactRunPattern.MatchString(request.RunID) || !artifact.ValidID(request.ArtifactID) {
		return artifact.ReadRequest{}, errors.New("Hub 的产物读取请求格式无效")
	}
	return request, nil
}

// ReportArtifact 仅向已配置的 Hub 上传本次请求的原始快照或读取失败状态。
func (client *Client) ReportArtifact(ctx context.Context, identity Identity, requestID, status string, body []byte) error {
	if !artifact.ValidReadID(requestID) || len(body) > artifact.MaxSize ||
		status != "available" && (len(body) != 0 || status != "missing" && status != "failed") {
		return errors.New("无效的产物读取回执")
	}
	endpoint, err := client.deviceURL(identity, "artifact-reads", requestID)
	if err != nil {
		return err
	}
	endpoint.RawQuery = url.Values{"status": {status}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+identity.Token)
	if status == "available" {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	// 沿用 Hub 的连接和重定向策略，仅为有大小上限的快照上传延长时限。
	upload := *client.http
	upload.Timeout = 40 * time.Second
	response, err := upload.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		return &HTTPError{Status: response.StatusCode}
	}
}
