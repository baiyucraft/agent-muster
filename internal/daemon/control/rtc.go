package control

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/baiyucraft/agent-muster/internal/peer"
)

// RegisterRTC 登记或续期当前设备的信令可用状态，并获取 Hub 下发的 ICE 配置。
func (client *Client) RegisterRTC(ctx context.Context, id Identity) (peer.RTCConfig, error) {
	var config peer.RTCConfig
	err := client.collaboration(ctx, id, http.MethodPut, nil, &config, "rtc")
	return config, err
}

// RTCOffers 查询发给当前设备的待应答连接提议。
func (client *Client) RTCOffers(ctx context.Context, id Identity) ([]peer.RTCOffer, error) {
	var offers peer.RTCOffers
	err := client.collaboration(ctx, id, http.MethodGet, nil, &offers, "rtc", "offers")
	return offers.Offers, err
}

// PutRTCOffer 由发送设备发布指定投递的连接提议，并为该次尝试绑定 SDP。
func (client *Client) PutRTCOffer(ctx context.Context, id Identity, transfer string, signal peer.RTCSignal) error {
	return client.collaboration(ctx, id, http.MethodPut, signal, nil, "rtc", transfer, "offer")
}

// RTCAnswer 由发送设备查询指定尝试的连接应答。空 SDP 表示应答仍在等待。
func (client *Client) RTCAnswer(ctx context.Context, id Identity, transfer, attempt string) (peer.RTCAnswer, error) {
	var answer peer.RTCAnswer
	endpoint, err := client.deviceURL(id, "rtc", transfer, "answer")
	if err != nil {
		return answer, err
	}
	query := endpoint.Query()
	query.Set("attempt_id", attempt)
	endpoint.RawQuery = query.Encode()
	response, err := client.send(ctx, http.MethodGet, endpoint, id.Token, nil, http.StatusOK)
	if err != nil {
		return answer, err
	}
	defer response.Body.Close()
	err = json.NewDecoder(http.MaxBytesReader(nil, response.Body, 2*peer.MaxSDPBytes+256)).Decode(&answer)
	return answer, err
}

// PutRTCAnswer 由目标设备提交与当前连接尝试匹配的 SDP 应答。
func (client *Client) PutRTCAnswer(ctx context.Context, id Identity, transfer string, signal peer.RTCSignal) error {
	return client.collaboration(ctx, id, http.MethodPut, signal, nil, "rtc", transfer, "answer")
}
