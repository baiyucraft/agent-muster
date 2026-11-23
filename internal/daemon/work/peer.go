package work

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
)

var transferID = regexp.MustCompile(`^transfer-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var overlayNetwork = netip.MustParsePrefix("100.64.0.0/10")

// PeerConfig 配置设备间传输的可选 HTTPS 地址和状态回调。
type PeerConfig struct {
	// Address 为空时由 WebRTC 自动建立设备间连接。
	Address string
	// OnStatus 可能由收发两侧并发调用。ready 表示已登记信令，direct 或 relay 表示已选定传输路径。
	OnStatus func(string)
}

type collaborationHub interface {
	hub
	RegisterPeer(context.Context, control.Identity, string, string) error
	RenewRun(context.Context, control.Identity, string, control.LeaseProof) (time.Time, error)
	RunStopRequested(context.Context, control.Identity, string, control.LeaseProof) (bool, error)
	Delegate(context.Context, control.Identity, string, control.Delegation) (json.RawMessage, error)
	ValidateWait(context.Context, control.Identity, string, control.RunEvent) error
	CreateTransfer(context.Context, control.Identity, string, control.Manifest) (control.Transfer, error)
	AuthorizeTransfer(context.Context, control.Identity, string) (control.Transfer, error)
	VerifyTransfer(context.Context, control.Identity, string, string) (control.Transfer, error)
	AckTransfer(context.Context, control.Identity, string, string) error
	ReportTransferError(context.Context, control.Identity, string, string) error
}

type collaboration struct {
	worker      *Worker
	client      collaborationHub
	identity    control.Identity
	address     string
	fingerprint string
	server      *http.Server
	rtc         *rtcTransport
	publishMu   sync.Mutex
	delegateMu  sync.Mutex
}

func openPeer(ctx context.Context, worker *Worker, hub collaborationHub, id control.Identity, config PeerConfig) (*collaboration, error) {
	peer := &collaboration{worker: worker, client: hub, identity: id}
	// 提前初始化自动互联，可选 HTTPS 绑定失败时调用方仍可启动 WebRTC。
	if signalHub, ok := hub.(rtcHub); ok {
		peer.rtc = newRTCTransport(peer, signalHub, config.OnStatus)
	}
	if config.Address == "" {
		return peer, nil
	}
	origin, err := peerOrigin(config.Address)
	if err != nil {
		return peer, err
	}
	certificate, err := worker.peerCertificate(ctx)
	if err != nil {
		return peer, err
	}
	listener, err := net.Listen("tcp", origin.Host)
	if err != nil {
		return peer, err
	}
	peer.address = origin.String()
	peer.fingerprint = transferHash(certificate.Leaf.RawSubjectPublicKeyInfo)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /peer/v1/transfers/{id}", peer.receive)
	peer.server = &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 45 * time.Second,
		WriteTimeout: 50 * time.Second, IdleTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}},
	}
	go peer.server.Serve(tls.NewListener(listener, peer.server.TLSConfig))
	return peer, nil
}

func (peer *collaboration) close() {
	if peer.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := peer.server.Shutdown(ctx); err != nil {
			_ = peer.server.Close()
		}
	}
}

// Run 保持设备可接收信令并重试发件箱，收到取消信号后等待信令监听退出。
func (peer *collaboration) Run(ctx context.Context, onError func(error)) {
	if !peer.enabled() {
		return
	}
	if peer.rtc != nil {
		rtcCtx, cancelRTC := context.WithCancel(ctx)
		var listeners sync.WaitGroup
		listeners.Go(func() { peer.rtc.listen(rtcCtx, onError) })
		if peer.address != "" {
			listeners.Go(func() { peer.registerHTTPS(rtcCtx, onError) })
		}
		defer func() {
			cancelRTC()
			listeners.Wait()
		}()
	} else if !peer.registerHTTPS(ctx, onError) {
		return
	}
	for ctx.Err() == nil {
		err := peer.sendPending(ctx)
		if onError != nil && err != nil {
			onError(err)
		}
		if errors.Is(err, control.ErrUnauthorized) {
			return
		}
		if !sleepContext(ctx, 3*time.Second) {
			return
		}
	}
}

func (peer *collaboration) registerHTTPS(ctx context.Context, onError func(error)) bool {
	delay := time.Second
	for ctx.Err() == nil {
		err := peer.client.RegisterPeer(ctx, peer.identity, peer.address, peer.fingerprint)
		if err == nil {
			return true
		}
		if onError != nil && ctx.Err() == nil {
			onError(err)
		}
		if errors.Is(err, control.ErrUnauthorized) || !sleepContext(ctx, delay) {
			return false
		}
		delay = min(2*delay, 30*time.Second)
	}
	return false
}

func (peer *collaboration) enabled() bool {
	return peer != nil && (peer.address != "" || peer.rtc != nil)
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (worker *Worker) peerCertificate(ctx context.Context) (tls.Certificate, error) {
	var certPEM, keyPEM []byte
	err := worker.db.QueryRowContext(ctx, "SELECT cert,key FROM peer_credentials WHERE id=1").Scan(&certPEM, &keyPEM)
	if errors.Is(err, sql.ErrNoRows) {
		key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			return tls.Certificate{}, e
		}
		serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if e != nil {
			return tls.Certificate{}, e
		}
		now := time.Now()
		template := &x509.Certificate{SerialNumber: serial, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		cert, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if e != nil {
			return tls.Certificate{}, e
		}
		private, e := x509.MarshalPKCS8PrivateKey(key)
		if e != nil {
			return tls.Certificate{}, e
		}
		certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
		_, err = worker.db.ExecContext(ctx, "INSERT INTO peer_credentials VALUES(1,?,?)", certPEM, keyPEM)
	}
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func peerOrigin(raw string) (*url.URL, error) {
	address, err := url.Parse(raw)
	if err != nil || address.Scheme != "https" || address.User != nil || address.RawQuery != "" || address.ForceQuery || address.Fragment != "" || address.Path != "" && address.Path != "/" {
		return nil, errors.New("设备互联地址须为 HTTPS IP:端口")
	}
	host, port, err := net.SplitHostPort(address.Host)
	if err != nil || port == "" {
		return nil, errors.New("设备互联地址须包含明确的端口")
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return nil, errors.New("无效的设备互联端口")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !(ip.IsLoopback() || ip.IsPrivate() || overlayNetwork.Contains(ip)) {
		return nil, errors.New("设备互联地址须使用 LAN 或 VPN IP")
	}
	address.Path = ""
	return address, nil
}

// NormalizePeerAddress 校验可选的 HTTPS 互联地址，空值表示停用手工地址。
func NormalizePeerAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", nil
	}
	endpoint, err := peerOrigin(address)
	if err != nil {
		return "", err
	}
	return endpoint.String(), nil
}

func transferHash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (peer *collaboration) receive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if peer.rtc != nil && (peer.rtc.authErr != nil || peer.rtc.pinned.transfer == id) {
		http.Error(w, "transfer requires authenticated WebRTC", http.StatusForbidden)
		return
	}
	ticket, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !transferID.MatchString(id) || !ok || len(ticket) == 0 || len(ticket) > 4096 {
		http.Error(w, "invalid transfer", http.StatusBadRequest)
		return
	}
	manifest, err := peer.verifyInbound(r.Context(), id, ticket)
	if err != nil {
		http.Error(w, "transfer authorization rejected", http.StatusUnauthorized)
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != manifest.Size {
		http.Error(w, "transfer size does not match", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, manifest.Size+1))
	if err != nil {
		http.Error(w, "transfer body invalid", http.StatusBadRequest)
		return
	}
	err = peer.commitInbound(r.Context(), manifest, ticket, body)
	switch {
	case errors.Is(err, errInboundDigest):
		http.Error(w, "transfer digest does not match", http.StatusBadRequest)
	case errors.Is(err, errInboundConflict):
		http.Error(w, "transfer identity conflict", http.StatusConflict)
	case err != nil:
		http.Error(w, "transfer acknowledgement pending", http.StatusServiceUnavailable)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

var (
	errInboundDigest   = errors.New("投递正文大小或摘要不符")
	errInboundConflict = errors.New("本地收件箱已有不同投递记录")
)

func (peer *collaboration) verifyInbound(ctx context.Context, id, ticket string) (control.Transfer, error) {
	manifest, err := peer.client.VerifyTransfer(ctx, peer.identity, id, ticket)
	if err != nil {
		return manifest, err
	}
	if manifest.ID != id || manifest.TargetDeviceID != peer.identity.DeviceID || manifest.Size < 0 || manifest.Size > control.MaxPeerBody {
		return manifest, errors.New("设备投递记录无效")
	}
	return manifest, nil
}

func (peer *collaboration) commitInbound(ctx context.Context, manifest control.Transfer, ticket string, body []byte) error {
	if int64(len(body)) != manifest.Size || transferHash(body) != manifest.SHA256 {
		return errInboundDigest
	}
	// 先提交本地收件箱再向 Hub 确认。确认丢失时，相同投递 ID 和清单可安全重放。
	result, err := peer.worker.db.ExecContext(ctx, `
		INSERT INTO peer_inbox(id,name,kind,sha256,size,body) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET id=excluded.id
		WHERE (peer_inbox.name,peer_inbox.kind,peer_inbox.sha256,peer_inbox.size)=(excluded.name,excluded.kind,excluded.sha256,excluded.size)
	`, manifest.ID, manifest.Name, manifest.Kind, manifest.SHA256, manifest.Size, body)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errInboundConflict
	}
	if err := peer.client.AckTransfer(ctx, peer.identity, manifest.ID, ticket); err != nil {
		return err
	}
	peer.worker.notify()
	return nil
}

func (peer *collaboration) sendPending(ctx context.Context) error {
	// Hub 确认投递清单后，设备才能为本地发件箱的正文申请发送授权。
	rows, err := peer.worker.db.QueryContext(ctx, `SELECT id FROM peer_manifests WHERE published=0 ORDER BY rowid`)
	if err != nil {
		return err
	}
	var unpublished []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		unpublished = append(unpublished, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var pendingErr error
	for _, id := range unpublished {
		if _, err := peer.publishTransfer(ctx, id); err != nil {
			pendingErr = errors.Join(pendingErr, err)
		}
	}
	rows, err = peer.worker.db.QueryContext(ctx, `SELECT b.id FROM peer_outbox b JOIN peer_manifests m ON m.id=b.id WHERE m.published=1 ORDER BY b.rowid`)
	if err != nil {
		return errors.Join(pendingErr, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		if err := peer.sendOne(ctx, id); err != nil {
			pendingErr = errors.Join(pendingErr, err)
		}
	}
	return pendingErr
}

func (peer *collaboration) sendOne(ctx context.Context, id string) error {
	var body []byte
	err := peer.worker.db.QueryRowContext(ctx, "SELECT body FROM peer_outbox WHERE id=?", id).Scan(&body)
	if err != nil {
		return err
	}
	current, err := peer.client.AuthorizeTransfer(ctx, peer.identity, id)
	if err != nil {
		return err
	}
	if current.ID != id {
		return errors.New("Hub 发回的投递标识与本地发件箱不一致")
	}
	// 先查 Hub 的收件记录，完成过的投递可直接清理本地发件箱。
	if current.ReceivedAt == "" {
		if err := peer.push(ctx, current, body); err != nil {
			reason := "delivery_failed"
			if errors.Is(err, errPeerVerification) {
				reason = "verification_failed"
			}
			_ = peer.client.ReportTransferError(ctx, peer.identity, id, reason)
			return err
		}
	}
	_, err = peer.worker.db.ExecContext(ctx, "DELETE FROM peer_outbox WHERE id=?", id)
	return err
}

func (peer *collaboration) push(ctx context.Context, manifest control.Transfer, body []byte) error {
	if peer.rtc != nil && (peer.rtc.authErr != nil || peer.rtc.pinned.transfer == manifest.ID) {
		return peer.rtc.push(ctx, manifest, body)
	}
	if manifest.Peer != nil {
		err := pushTransfer(ctx, manifest, body)
		if err == nil && peer.rtc != nil {
			peer.rtc.status("direct")
		}
		// TLS 指纹不匹配时保留身份校验错误。其他 HTTPS 失败可回退到 WebRTC。
		if err == nil || peer.rtc == nil || errors.Is(err, errPeerVerification) {
			return err
		}
	}
	if peer.rtc != nil {
		return peer.rtc.push(ctx, manifest, body)
	}
	return errors.New("接收设备尚未登记设备互联地址")
}

var errPeerVerification = errors.New("接收设备 TLS 证书验证失败")

func pushTransfer(ctx context.Context, manifest control.Transfer, body []byte) error {
	if manifest.Peer == nil || manifest.Ticket == "" || manifest.Peer.DeviceID != manifest.TargetDeviceID ||
		!transferID.MatchString(manifest.ID) || int64(len(body)) != manifest.Size || transferHash(body) != manifest.SHA256 {
		return errors.New("发送内容与投递记录不匹配")
	}
	origin, err := peerOrigin(manifest.Peer.Address)
	if err != nil {
		return err
	}
	fingerprint, err := hex.DecodeString(manifest.Peer.Fingerprint)
	if err != nil || len(fingerprint) != sha256.Size {
		return errors.New("设备证书指纹格式无效")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // 使用下方的公钥指纹校验匹配已登记设备。
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errPeerVerification
			}
			digest := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(digest[:], fingerprint) != 1 {
				return errPeerVerification
			}
			return nil
		},
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, origin.JoinPath("peer", "v1", "transfers", manifest.ID).String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+manifest.Ticket)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("设备接收投递返回 HTTP %d", response.StatusCode)
	}
	return nil
}
