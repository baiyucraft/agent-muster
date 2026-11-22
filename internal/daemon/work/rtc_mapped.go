package work

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// newMappedRTCConnection 通过已经映射的本机 IPv4 socket 发布设备候选。
// 调用时接管 socket，出错时关闭。成功时由返回的清理函数关闭连接和 socket。
func newMappedRTCConnection(
	hubURL string, servers []webrtc.ICEServer, socket *net.UDPConn, mapped netip.AddrPort,
) (*webrtc.PeerConnection, func(), error) {
	if socket == nil {
		return nil, nil, errors.New("设备端口映射缺少本机 UDP socket")
	}
	bound, ok := socket.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, nil, errors.New("设备端口映射缺少本机 UDP 地址")
	}
	local := bound.AddrPort()
	localIP := local.Addr().Unmap()
	mappedIP := mapped.Addr().Unmap()
	if !localIP.Is4() || localIP.IsUnspecified() || local.Port() == 0 ||
		!mappedIP.Is4() || mappedIP.IsUnspecified() || mapped.Port() == 0 || socket.RemoteAddr() != nil {
		_ = socket.Close()
		return nil, nil, errors.New("设备端口映射需要已绑定的非连接 IPv4 UDP socket")
	}
	if err := socket.SetDeadline(time.Time{}); err != nil {
		_ = socket.Close()
		return nil, nil, err
	}

	settings := rtcSettings(hubURL)
	mode := webrtc.ICEAddressRewriteAppend
	if local.Port() != mapped.Port() {
		// Pion 将端口改写应用到同一 socket 的所有匹配候选。本机地址不能使用映射端口。
		mode = webrtc.ICEAddressRewriteReplace
	}
	if err := settings.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
		Local:           localIP.String(),
		External:        []string{mappedIP.String()},
		AsCandidateType: webrtc.ICECandidateTypeHost,
		Mode:            mode,
		Networks:        []webrtc.NetworkType{webrtc.NetworkTypeUDP4},
		OriginalPort:    int(local.Port()),
		NewPort:         int(mapped.Port()),
	}); err != nil {
		_ = socket.Close()
		return nil, nil, err
	}
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: socket})
	settings.SetICEUDPMux(mux)
	connection, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{ICEServers: servers})
	if err != nil {
		_ = mux.Close()
		return nil, nil, err
	}
	closeConnection := sync.OnceFunc(func() {
		_ = connection.Close()
		_ = mux.Close()
	})
	return connection, closeConnection, nil
}
