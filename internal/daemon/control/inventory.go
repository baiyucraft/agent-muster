// Package control 提供设备端调用 Hub 接口的客户端。
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

const (
	// HeartbeatInterval 是设备心跳和更新检查的间隔。
	HeartbeatInterval = 30 * time.Second
	// InventoryInterval 是本机 CLI 定期探测的间隔。
	InventoryInterval = 5 * time.Minute
)

var (
	// ErrUnauthorized 表示 Hub 已拒绝该设备凭据。
	ErrUnauthorized = errors.New("Hub 拒绝设备凭据，请重新配对")
	pairCodePattern = regexp.MustCompile("^[A-Za-z0-9_-]{24}$")
	deviceIDPattern = regexp.MustCompile("^d-[a-f0-9]{32}$")
	tokenPattern    = regexp.MustCompile("^[A-Za-z0-9_-]{43}$")
)

// Identity 保存配对得到的设备 ID、令牌和签发它的 Hub 地址。
type Identity struct {
	HubURL   string `json:"hub_url"`
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
}

// Client 向同一个 Hub 发送设备配对及已认证的设备请求。
type Client struct {
	http *http.Client
	base *url.URL
}

// NewClient 使用显式配置的 HTTP 或 HTTPS 地址。请求停留在该地址，遇到重定向时返回原响应。
func NewClient(hubURL string) (*Client, error) {
	const badURL = "-hub-url 须为有效的 Hub 地址"
	base, err := url.Parse(hubURL)
	if err != nil || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || len(hubURL) > 1024 {
		return nil, errors.New(badURL)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if base.Scheme == "http" {
		host := base.Hostname()
		transport.Proxy = nil
		if strings.EqualFold(host, "localhost") {
			// localhost 的 HTTP 请求固定走回环地址，避免受本机 hosts 配置影响。
			dialer := &net.Dialer{}
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
				if err != nil && ctx.Err() == nil {
					return dialer.DialContext(ctx, network, net.JoinHostPort("::1", port))
				}
				return conn, err
			}
		}
	} else if base.Scheme != "https" {
		return nil, errors.New("-hub-url 须使用 HTTP 或 HTTPS")
	}
	return &Client{
		base: base,
		http: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Valid 检查设备 ID 和令牌的格式，凭据是否仍有效由 Hub 判断。
func (identity Identity) Valid() bool {
	return deviceIDPattern.MatchString(identity.DeviceID) && tokenPattern.MatchString(identity.Token)
}

// Enroll 向当前 Hub 兑换一次性配对码，返回绑定到该 Hub 的设备凭据。
func (client *Client) Enroll(ctx context.Context, code, hostname string) (Identity, error) {
	if !pairCodePattern.MatchString(code) {
		return Identity{}, errors.New("配对码格式无效")
	}
	payload := struct {
		Code     string `json:"code"`
		Hostname string `json:"hostname"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
	}{Code: code, Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH}
	endpoint := client.base.JoinPath("api", "v1", "devices", "enroll")
	response, err := client.send(ctx, http.MethodPost, endpoint, "", payload, http.StatusCreated)
	if err != nil {
		return Identity{}, err
	}
	defer response.Body.Close()

	var identity Identity
	const maxResponse = 4096
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse || json.Unmarshal(data, &identity) != nil || !identity.Valid() {
		return Identity{}, errors.New("Hub 的配对响应格式无效")
	}
	identity.HubURL = client.base.String()
	return identity, nil
}

// Heartbeat 刷新已配对设备在 Hub 的最后在线时间。
func (client *Client) Heartbeat(ctx context.Context, identity Identity) error {
	return client.report(ctx, identity, http.MethodPost, "heartbeat", nil)
}

// ReportInventory 上报一次完整的本机 Agent 探测快照。
func (client *Client) ReportInventory(ctx context.Context, identity Identity, agents []runner.Agent) error {
	payload := struct {
		Agents []runner.Agent `json:"agents"`
	}{Agents: agents}
	return client.report(ctx, identity, http.MethodPut, "inventory", payload)
}

func (client *Client) report(ctx context.Context, identity Identity, method, operation string, payload any) error {
	if !identity.Valid() || identity.HubURL != client.base.String() {
		return errors.New("保存的设备凭据格式无效")
	}
	endpoint := client.base.JoinPath("api", "v1", "devices", identity.DeviceID, operation)
	response, err := client.send(ctx, method, endpoint, identity.Token, payload, http.StatusNoContent)
	if err == nil {
		response.Body.Close()
	}
	return err
}

func (client *Client) send(ctx context.Context, method string, endpoint *url.URL, token string, payload any, statusCodes ...int) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return nil, err
	}
	for _, statusCode := range statusCodes {
		if response.StatusCode == statusCode {
			return response, nil
		}
	}
	response.Body.Close()
	if token != "" && response.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	return nil, &HTTPError{Status: response.StatusCode}
}
