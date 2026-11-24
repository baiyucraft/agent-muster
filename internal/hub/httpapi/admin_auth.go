package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/adminauth"
	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"golang.org/x/time/rate"
)

const (
	adminSessionCookie = "muster_admin_session"
	adminSessionPath   = "/api/v1"
	adminSessionTTL    = 24 * time.Hour
	loginRefill        = 10 * time.Second
	loginBurst         = 5
	maxLoginSources    = 4096
)

// AdminSessionStore 提供单管理员凭据和会话存储，只有令牌摘要写入数据库。
// 登录签发时需要再次核对密码哈希，以防重置密码的同时签发旧凭据会话。
type AdminSessionStore interface {
	AdminCredentials(context.Context) (username, passwordHash string, err error)
	CreateAdminSession(context.Context, string, [32]byte, time.Time) error
	ValidateAdminSession(context.Context, [32]byte) error
	DeleteAdminSession(context.Context, [32]byte) error
}

// AdminAuthOptions 启用 Web 管理员登录。SecureCookie 适用于在反向代理处终止 HTTPS 的部署。
type AdminAuthOptions struct {
	Store        AdminSessionStore
	SecureCookie bool
}

type adminAuth struct {
	AdminAuthOptions
	origin http.CrossOriginProtection
	limits loginLimits
}

type loginLimits struct {
	mu        sync.Mutex
	sources   map[netip.Addr]loginSource
	nextSweep time.Time
	// 无法单独跟踪的来源共享备用桶，限制来源表的内存开销。
	overflow *rate.Limiter
	hashing  chan struct{}
}

type loginSource struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newAdminAuth(options AdminAuthOptions) *adminAuth {
	return &adminAuth{AdminAuthOptions: options, limits: loginLimits{
		sources: make(map[netip.Addr]loginSource), overflow: rate.NewLimiter(rate.Every(loginRefill), loginBurst),
		hashing: make(chan struct{}, 2),
	}}
}

func (limits *loginLimits) allow(remoteAddr string) bool {
	host, _, _ := net.SplitHostPort(remoteAddr)
	address, _ := netip.ParseAddr(host)
	address = address.Unmap()
	if address.Is6() {
		address = netip.PrefixFrom(address, 64).Masked().Addr()
	}
	now := time.Now()
	limits.mu.Lock()
	defer limits.mu.Unlock()
	if !now.Before(limits.nextSweep) {
		for address, source := range limits.sources {
			if now.Sub(source.lastSeen) >= time.Minute {
				delete(limits.sources, address)
			}
		}
		limits.nextSweep = now.Add(time.Minute)
	}
	source, found := limits.sources[address]
	if !found {
		if !address.IsValid() || len(limits.sources) >= maxLoginSources {
			return limits.overflow.AllowN(now, 1)
		}
		source.limiter = rate.NewLimiter(rate.Every(loginRefill), loginBurst)
	}
	source.lastSeen = now
	limits.sources[address] = source
	return source.limiter.AllowN(now, 1)
}

func (auth *adminAuth) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/login", auth.protect(auth.login))
	mux.HandleFunc("GET /api/v1/auth/session", auth.session)
	mux.HandleFunc("POST /api/v1/auth/logout", auth.protect(auth.logout))
}

func (auth *adminAuth) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth.origin.Check(r) != nil {
			writeError(w, http.StatusForbidden, "cross-origin request denied")
			return
		}
		next(w, r)
	}
}

func (auth *adminAuth) login(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	// JSON 转义可能占用最多六倍的字节数，限制整个请求仍保留合法密码的表示空间。
	if !readJSONLimit(w, r, &request, 8<<10) {
		return
	}
	if !adminauth.ValidUsername(request.Username) || !adminauth.ValidPassword(request.Password) {
		writeError(w, http.StatusUnauthorized, "invalid administrator credentials")
		return
	}
	// 限速按直接连接来源计算。密码哈希并发另行限制以保护 Hub 内存。
	if !auth.limits.allow(r.RemoteAddr) {
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}
	select {
	case auth.limits.hashing <- struct{}{}:
		defer func() { <-auth.limits.hashing }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}
	if auth.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "administrator account not configured")
		return
	}
	username, passwordHash, err := auth.Store.AdminCredentials(r.Context())
	if errors.Is(err, registry.ErrUnauthorized) {
		writeError(w, http.StatusServiceUnavailable, "administrator account not configured")
		return
	}
	if err != nil {
		writeAdminAuthError(w, err)
		return
	}
	if !adminauth.VerifyPassword(passwordHash, request.Password) || request.Username != username {
		writeError(w, http.StatusUnauthorized, "invalid administrator credentials")
		return
	}
	secret, err := randomBytes(tokenBytes)
	if err != nil {
		writeAdminAuthError(w, err)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	expires := time.Now().Add(adminSessionTTL)
	if err := auth.Store.CreateAdminSession(r.Context(), passwordHash, sha256.Sum256([]byte(token)), expires); err != nil {
		writeAdminAuthError(w, err)
		return
	}
	cookie := auth.cookie(r, token)
	cookie.MaxAge = int(adminSessionTTL.Seconds())
	cookie.Expires = expires
	http.SetCookie(w, cookie)
	w.WriteHeader(http.StatusNoContent)
}

func (auth *adminAuth) validate(r *http.Request) error {
	hash, ok := adminSessionHash(r)
	if !ok || auth.Store == nil {
		return registry.ErrUnauthorized
	}
	return auth.Store.ValidateAdminSession(r.Context(), hash)
}

func (auth *adminAuth) session(w http.ResponseWriter, r *http.Request) {
	if err := auth.validate(r); err != nil {
		writeAdminAuthError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (auth *adminAuth) logout(w http.ResponseWriter, r *http.Request) {
	if hash, ok := adminSessionHash(r); ok && auth.Store != nil {
		if err := auth.Store.DeleteAdminSession(r.Context(), hash); err != nil {
			writeAdminAuthError(w, err)
			return
		}
	}
	cookie := auth.cookie(r, "")
	cookie.MaxAge = -1
	cookie.Expires = time.Unix(0, 0)
	http.SetCookie(w, cookie)
	w.WriteHeader(http.StatusNoContent)
}

func (auth *adminAuth) cookie(r *http.Request, token string) *http.Cookie {
	return &http.Cookie{
		Name: adminSessionCookie, Path: adminSessionPath, Value: token,
		HttpOnly: true, Secure: auth.SecureCookie || r.TLS != nil, SameSite: http.SameSiteStrictMode,
	}
}

func adminSessionHash(r *http.Request) ([32]byte, bool) {
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil || !validRandomSecret(cookie.Value, tokenBytes) {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(cookie.Value)), true
}

func writeAdminAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, registry.ErrUnauthorized) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	log.Printf("administrator authentication failed (%T)", err)
	writeError(w, http.StatusServiceUnavailable, "administrator authentication unavailable")
}
