package work

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

const rtcUPnPBodyLimit = 64 << 10

type rtcUPnPService struct {
	typeURN string
	control *url.URL
	client  *http.Client
}

type rtcUPnPDevice struct {
	Services []struct {
		Type    string `xml:"serviceType"`
		Control string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Children []rtcUPnPDevice `xml:"deviceList>device"`
}

type rtcUPnPResult struct {
	Address        string `xml:"NewExternalIPAddress"`
	InternalPort   uint16 `xml:"NewInternalPort"`
	InternalClient string `xml:"NewInternalClient"`
	Enabled        string `xml:"NewEnabled"`
	Description    string `xml:"NewPortMappingDescription"`
	Lease          uint32 `xml:"NewLeaseDuration"`
}

// rtcUPnPPortMap 只尝试默认网关的 IGD。空状态表示未发现可用的网关服务。
func rtcUPnPPortMap(ctx context.Context, local netip.AddrPort, gateway netip.Addr) (netip.AddrPort, string, func()) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ssdpEnd := netip.AddrPortFrom(gateway, 1900)
	location := rtcDiscoverUPnP(ctx, local.Addr(), ssdpEnd)
	if location == nil {
		return netip.AddrPort{}, "", func() {}
	}
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: local.Addr().AsSlice()}}
	transport := &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true, MaxResponseHeaderBytes: 16 << 10}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 750 * time.Millisecond,
	}
	service, ok := rtcReadUPnPDevice(ctx, gateway, location, client)
	if !ok {
		return netip.AddrPort{}, "", func() {}
	}
	return service.mapPort(ctx, local)
}

// rtcDiscoverUPnP 查询 IGD 的本地 SSDP 组播和网关单播，只接受默认网关的响应。
func rtcDiscoverUPnP(ctx context.Context, source netip.Addr, gateway netip.AddrPort) *url.URL {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	socket, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(source, 0)))
	if err != nil {
		return nil
	}
	defer socket.Close()
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if socket.SetDeadline(deadline) != nil {
		return nil
	}
	multicast := netip.MustParseAddrPort("239.255.255.250:1900")
	multicastOK := rtcSetUPnPMulticast(socket, source)
	for version := 1; version <= 2; version++ {
		message := fmt.Appendf(nil, "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:%d\r\n\r\n", version)
		_, _ = socket.WriteToUDPAddrPort(message, gateway)
		if multicastOK {
			_, _ = socket.WriteToUDPAddrPort(message, multicast)
		}
	}
	var datagram [4096]byte
	for {
		n, remote, err := socket.ReadFromUDPAddrPort(datagram[:])
		if err != nil {
			return nil
		}
		if remote.Addr() == gateway.Addr() {
			if address := rtcUPnPLocation(datagram[:n], gateway.Addr()); address != nil {
				return address
			}
		}
	}
}

// rtcSetUPnPMulticast 将 SSDP 组播限制在默认网关所在接口及本地一跳。
func rtcSetUPnPMulticast(socket *net.UDPConn, source netip.Addr) bool {
	raw, err := socket.SyscallConn()
	if err != nil {
		return false
	}
	var socketErr error
	err = raw.Control(func(fd uintptr) {
		handle := windows.Handle(fd)
		socketErr = windows.SetsockoptInet4Addr(handle, windows.IPPROTO_IP, windows.IP_MULTICAST_IF, source.As4())
		if socketErr == nil {
			socketErr = windows.SetsockoptInt(handle, windows.IPPROTO_IP, windows.IP_MULTICAST_TTL, 1)
		}
	})
	return err == nil && socketErr == nil
}

// rtcUPnPLocation 从成功的 SSDP 报文提取网关设备描述地址。
func rtcUPnPLocation(packet []byte, gateway netip.Addr) *url.URL {
	lines := strings.Split(string(packet), "\r\n")
	status := strings.Fields(lines[0])
	if len(status) < 2 || status[0] != "HTTP/1.1" || status[1] != "200" {
		return nil
	}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "location") {
			location, err := url.Parse(strings.TrimSpace(value))
			if err == nil && rtcUPnPGatewayURL(location, gateway) {
				return location
			}
		}
	}
	return nil
}

// rtcUPnPGatewayURL 只允许 HTTP 使用默认网关的 IPv4 字面地址。
func rtcUPnPGatewayURL(address *url.URL, gateway netip.Addr) bool {
	return address != nil && address.Scheme == "http" && address.User == nil && address.Opaque == "" &&
		address.Hostname() == gateway.String() && address.Fragment == ""
}

// rtcReadUPnPDevice 解析设备描述并选取位于网关上的 WAN 映射服务。
func rtcReadUPnPDevice(ctx context.Context, gateway netip.Addr, location *url.URL, client *http.Client) (rtcUPnPService, bool) {
	if !rtcUPnPGatewayURL(location, gateway) {
		return rtcUPnPService{}, false
	}
	var description struct {
		XMLName xml.Name      `xml:"root"`
		Base    string        `xml:"URLBase"`
		Device  rtcUPnPDevice `xml:"device"`
	}
	body, status := rtcUPnPHTTP(ctx, client, http.MethodGet, location, "", "")
	if status != http.StatusOK || xml.Unmarshal(body, &description) != nil {
		return rtcUPnPService{}, false
	}
	base := location
	if strings.TrimSpace(description.Base) != "" {
		var err error
		base, err = url.Parse(strings.TrimSpace(description.Base))
		if err != nil || !rtcUPnPGatewayURL(base, gateway) {
			return rtcUPnPService{}, false
		}
	}
	devices := []rtcUPnPDevice{description.Device}
	for len(devices) != 0 {
		device := devices[0]
		devices = append(devices[1:], device.Children...)
		for _, service := range device.Services {
			typ := strings.TrimSpace(service.Type)
			if typ != "urn:schemas-upnp-org:service:WANIPConnection:2" && typ != "urn:schemas-upnp-org:service:WANIPConnection:1" && typ != "urn:schemas-upnp-org:service:WANPPPConnection:1" {
				continue
			}
			target := strings.TrimSpace(service.Control)
			control, err := base.Parse(target)
			if err == nil && target != "" && rtcUPnPGatewayURL(control, gateway) {
				return rtcUPnPService{typeURN: typ, control: control, client: client}, true
			}
		}
	}
	return rtcUPnPService{}, false
}

// mapPort 检查外网地址和端口占用，再为当前 ICE UDP 端口申请短期租约。
func (service rtcUPnPService) mapPort(ctx context.Context, local netip.AddrPort) (netip.AddrPort, string, func()) {
	noop := func() {}
	current, fault := service.call(ctx, "GetExternalIPAddress", "")
	if fault != 0 {
		return netip.AddrPort{}, "unsupported", noop
	}
	address, err := netip.ParseAddr(strings.TrimSpace(current.Address))
	if err != nil {
		return netip.AddrPort{}, "unsupported", noop
	}
	if !rtcPublicMappedIPv4(address) {
		return netip.AddrPort{}, "private", noop
	}
	port := strconv.Itoa(int(local.Port()))
	key := "<NewRemoteHost></NewRemoteHost><NewExternalPort>" + port + "</NewExternalPort><NewProtocol>UDP</NewProtocol>"
	if _, fault := service.call(ctx, "GetSpecificPortMappingEntry", key); fault != 714 {
		return netip.AddrPort{}, "unsupported", noop
	}
	var nonce [12]byte
	_, _ = rand.Read(nonce[:])
	label := "Muster-" + hex.EncodeToString(nonce[:])
	args := key + "<NewInternalPort>" + port + "</NewInternalPort><NewInternalClient>" + local.Addr().String() +
		"</NewInternalClient><NewEnabled>1</NewEnabled><NewPortMappingDescription>" + label +
		"</NewPortMappingDescription><NewLeaseDuration>" + strconv.Itoa(rtcPortMapLeaseSeconds) + "</NewLeaseDuration>"
	release := sync.OnceFunc(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		mapping, fault := service.call(ctx, "GetSpecificPortMappingEntry", key)
		if fault == 0 && mapping.ownedBy(local, label) {
			service.call(ctx, "DeletePortMapping", key)
		}
	})
	if _, fault := service.call(ctx, "AddPortMapping", args); fault != 0 {
		// 即使 Add 的响应丢失，也只回收网关记录上匹配本次标识的租约。
		release()
		return netip.AddrPort{}, "unsupported", noop
	}
	mapping, fault := service.call(ctx, "GetSpecificPortMappingEntry", key)
	if fault != 0 || !mapping.ownedBy(local, label) || (mapping.Enabled != "1" && mapping.Enabled != "true") ||
		!rtcPortMapLeaseAllows(time.Duration(mapping.Lease)*time.Second) {
		release()
		return netip.AddrPort{}, "unsupported", noop
	}
	return netip.AddrPortFrom(address, local.Port()), "global", release
}

// ownedBy 要求描述和内部地址、端口均匹配本次租约，避免回收别的映射。
func (result rtcUPnPResult) ownedBy(local netip.AddrPort, description string) bool {
	return result.Description == description && result.InternalPort == local.Port() && result.InternalClient == local.Addr().String()
}

// call 提交固定的 SOAP 操作。返回 0 表示匹配操作的成功响应，-1 表示无法验证响应。
func (service rtcUPnPService) call(ctx context.Context, action, args string) (rtcUPnPResult, int) {
	var envelope struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			Response struct {
				XMLName xml.Name
				rtcUPnPResult
			} `xml:",any"`
			Fault struct {
				Code int `xml:"detail>UPnPError>errorCode"`
			} `xml:"Fault"`
		} `xml:"Body"`
	}
	message := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:` + action +
		` xmlns:u="` + service.typeURN + `">` + args + `</u:` + action + `></s:Body></s:Envelope>`
	body, status := rtcUPnPHTTP(ctx, service.client, http.MethodPost, service.control, service.typeURN+"#"+action, message)
	if xml.Unmarshal(body, &envelope) != nil {
		return rtcUPnPResult{}, -1
	}
	if status == http.StatusInternalServerError && envelope.Body.Fault.Code > 0 {
		return rtcUPnPResult{}, envelope.Body.Fault.Code
	}
	if status != http.StatusOK || envelope.Body.Response.XMLName.Local != action+"Response" || envelope.Body.Response.XMLName.Space != service.typeURN {
		return rtcUPnPResult{}, -1
	}
	return envelope.Body.Response.rtcUPnPResult, 0
}

// rtcUPnPHTTP 读取有大小限制的网关响应，请求只携带发现地址与 SOAP 字段。
func rtcUPnPHTTP(ctx context.Context, client *http.Client, method string, address *url.URL, soapAction, body string) ([]byte, int) {
	request, err := http.NewRequestWithContext(ctx, method, address.String(), strings.NewReader(body))
	if err != nil {
		return nil, 0
	}
	if soapAction != "" {
		request.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		request.Header.Set("SOAPAction", `"`+soapAction+`"`)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, rtcUPnPBodyLimit+1))
	if err != nil || len(data) > rtcUPnPBodyLimit {
		return nil, 0
	}
	return data, response.StatusCode
}
