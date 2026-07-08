package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"kiro-go/logger"
)

// reuseAddrListen binds a TCP listener with SO_REUSEADDR set on the socket before
// bind. Without it, on Windows the loopback callback port lingers in TIME_WAIT after
// the previous SSO login closes, so an immediate retry fails to rebind with "Only one
// usage of each socket address ... is normally permitted" (WSAEADDRINUSE). setReuseAddr
// is platform-specific (see reuseaddr_windows.go / reuseaddr_other.go).
func reuseAddrListen(addr string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var sockErr error
			if err := c.Control(func(fd uintptr) { sockErr = setReuseAddr(fd) }); err != nil {
				return err
			}
			return sockErr
		},
	}
	return lc.Listen(context.Background(), "tcp", addr)
}

// KiroSsoSession đại diện cho một phiên đăng nhập Kiro Hosted SSO đang diễn ra.
// Flow gồm 2 leg (theo zsecducna/cli-cache-proxy-api social.go):
//
//	Leg-1: Browser → app.kiro.dev/signin (PKCE) → portal phát hiện external IdP
//	       → redirect về http://localhost:<port>/?login_option=external_idp&issuer_url=...
//	Leg-2: Nhận IdP metadata → OIDC discovery → redirect browser đến Microsoft Entra (PKCE mới)
//	       → Microsoft redirect về http://localhost:<port>/oauth/callback?code=...
type KiroSsoSession struct {
	ID            string // session UUID
	State         string // OAuth state (CSRF protection, Leg-1)
	CodeVerifier  string // PKCE code verifier (Leg-1: Kiro portal)
	CodeChallenge string // PKCE code challenge (Leg-1)

	// Loopback server
	LoopbackPort   int
	LoopbackServer *http.Server
	LoopbackReady  chan struct{} // closed when loopback server is listening
	loopbackMu     sync.Mutex    // protects leg-2 single-shot dispatch

	// RedirectBase is the externally reachable base URL the IdP should redirect
	// the browser back to (e.g. "https://azr.hian.software" or "https://host:3128").
	// Empty means "http://localhost:<LoopbackPort>" (pure local flow). Used so the
	// OAuth redirect_uri is not hard-coded to localhost when the proxy is reached
	// through a custom domain / reverse proxy.
	RedirectBase string

	// IdP metadata (nhận từ Kiro portal redirect — không có sub-path, root redirect_uri)
	IssuerURL   string
	IdPClientID string
	IdPScopes   string
	LoginHint   string
	UserEmail   string

	// OIDC discovery results
	IdPAuthEndpoint  string
	IdPTokenEndpoint string

	// Leg-2 PKCE (cho external IdP — 96-byte verifier như zsec)
	IdPCodeVerifier string
	IdPState        string // CSRF state cho Leg-2

	// Single-shot gate: true sau khi external IdP descriptor đã được xử lý
	leg2Processing bool

	// idpAuthorizeURL lưu Microsoft authorize URL đã tính ở Leg-1 (cùng PKCE pair).
	// Cho phép Leg-1 idempotent: hit lặp lại với cùng state (vd: copy URL mở browser
	// ẩn danh) sẽ re-redirect tới đúng URL này thay vì trả 204, nên browser nào hoàn
	// tất login Microsoft cũng được.
	idpAuthorizeURL string

	// Kết quả cuối cùng
	ResultCh chan KiroSsoResult

	ExpiresAt time.Time
}

// KiroSsoResult chứa kết quả của flow Kiro SSO.
type KiroSsoResult struct {
	AccessToken      string
	RefreshToken     string
	ExpiresIn        int
	IssuerURL        string
	IdPClientID      string
	Scopes           string
	LoginHint        string
	UserEmail        string
	IdPTokenEndpoint string // cached token endpoint from OIDC discovery
	Err              error
}

var (
	kiroSsoSessions   = make(map[string]*KiroSsoSession)
	kiroSsoSessionsMu sync.RWMutex
)

// Kiro portal URLs (test-replaceable).
// Verified: app.kiro.dev is the Sign-in portal (Kiro firewall docs).
// prod.us-east-1.auth.desktop.kiro.dev is for token exchange/refresh only.
var kiroPortalSignInURL = "https://app.kiro.dev/signin"

// redirect_from value — KiroIDE for IDE flow (zsec SocialRedirectFrom constant).
var kiroRedirectFrom = "KiroIDE"

// externalIdpTokenURLFn resolves the IdP token endpoint from issuer URL.
// Mặc định: OIDC discovery qua client được truyền vào (proxy-aware ở refresh path).
// Test có thể thay thế qua SetExternalIdpTokenURLFnForTest.
var externalIdpTokenURLFn func(issuerURL string, client *http.Client) (string, error)

// kiroLoopbackPorts là tập port loopback chính thức mà Kiro portal chấp nhận cho
// Leg-1 redirect (verified: Kiro Okta IdP docs). bindKiroLoopback thử lần lượt theo
// đúng thứ tự này và dùng port trống đầu tiên.
var kiroLoopbackPorts = []int{3128, 4649, 6588, 8008, 9091, 49153, 50153, 51153, 52153, 53153}

// kiroLoopbackPortsOverride là test-only hook. Khi không rỗng, bindKiroLoopback dùng
// danh sách này thay cho kiroLoopbackPorts. Đặt {0} để ép dùng port ephemeral ngẫu nhiên,
// tránh xung đột port khi chạy test song song. Xem SetKiroLoopbackPortsForTest.
var kiroLoopbackPortsOverride []int

// bindKiroLoopback thử bind 127.0.0.1 lần lượt trên từng port trong kiroLoopbackPorts
// (hoặc kiroLoopbackPortsOverride khi đang test), trả về listener và port đầu tiên bind
// thành công. Nếu tất cả port đều bận, trả về lỗi rõ ràng.
//
// Khi chạy trong Docker, set env LOOPBACK_HOST=0.0.0.0 để loopback server có thể nhận
// callback từ browser bên ngoài container. Mặc định dùng 127.0.0.1 để bảo mật.
func bindKiroLoopback() (net.Listener, int, error) {
	ports := kiroLoopbackPorts
	if len(kiroLoopbackPortsOverride) > 0 {
		ports = kiroLoopbackPortsOverride
	}

	host := "127.0.0.1"
	if v := strings.TrimSpace(os.Getenv("LOOPBACK_HOST")); v != "" {
		host = v
		// L5: binding the OAuth callback listener to a non-loopback address exposes
		// it on the network for the ~10-minute login window. State/PKCE still guard
		// against CSRF, but warn so an operator who set this for Docker convenience
		// knows the trade-off; a reverse proxy + RedirectBase is the safer pattern.
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			logger.Warnf("[KiroSSO] LOOPBACK_HOST=%q binds the OAuth callback on a non-loopback "+
				"interface — it is reachable from the network during login. Prefer 127.0.0.1 with a "+
				"reverse proxy + RedirectBase for Docker.", host)
		}
	}

	for _, p := range ports {
		ln, err := reuseAddrListen(fmt.Sprintf("%s:%d", host, p))
		if err == nil {
			return ln, ln.Addr().(*net.TCPAddr).Port, nil
		}
	}

	return nil, 0, fmt.Errorf("tất cả port loopback Kiro (%v) đều đang bận", ports)
}

// serveLoopback chạy loopback HTTP server trên cả IPv4 và (best-effort) IPv6.
//
// redirect_uri của cả hai leg dùng host `localhost` (khớp đăng ký Microsoft Entra
// `http://localhost/oauth/callback`). Vì browser có thể resolve `localhost` thành
// 127.0.0.1 hoặc ::1, ta phục vụ trên cả hai để callback luôn tới được server.
//
//   - 127.0.0.1 (IPv4): bắt buộc — listener đã được bindKiroLoopback bind sẵn, truyền vào
//     qua tham số v4Listener. Nếu server này dừng bất thường (không phải ErrServerClosed)
//     thì đẩy lỗi vào ResultCh.
//   - [::1] (IPv6): best-effort — thử bind trên CÙNG port; nếu fail thì log debug và bỏ qua
//     (không làm hỏng flow). Nếu bind thành công thì phục vụ cùng handler.
//
// Hàm này block trên việc serve IPv4 listener (gọi trong goroutine ở StartKiroSsoLogin).
func serveLoopback(s *KiroSsoSession, v4Listener net.Listener, port int) {
	// IPv6 loopback best-effort trên cùng port.
	if v6Listener, err := reuseAddrListen(fmt.Sprintf("[::1]:%d", port)); err != nil {
		logger.Debugf("[KiroSSO] bind IPv6 loopback [::1]:%d thất bại (best-effort, bỏ qua): %v", port, err)
	} else {
		go func() {
			if err := s.LoopbackServer.Serve(v6Listener); err != nil && err != http.ErrServerClosed {
				logger.Debugf("[KiroSSO] IPv6 loopback server dừng: %v", err)
			}
		}()
	}

	// IPv4 loopback (bắt buộc) — block cho tới khi server dừng.
	if err := s.LoopbackServer.Serve(v4Listener); err != nil && err != http.ErrServerClosed {
		select {
		case s.ResultCh <- KiroSsoResult{Err: fmt.Errorf("loopback server stopped: %w", err)}:
		default:
		}
	}
}

// redirectBaseURL returns the base used to build OAuth redirect_uri values.
// When RedirectBase is set (custom domain / reverse proxy), it is used verbatim
// (the reverse proxy is responsible for routing it to this loopback port).
// Otherwise it falls back to http://localhost:<LoopbackPort> for the pure local flow.
func (s *KiroSsoSession) redirectBaseURL() string {
	if s.RedirectBase != "" {
		return strings.TrimRight(s.RedirectBase, "/")
	}
	return fmt.Sprintf("http://localhost:%d", s.LoopbackPort)
}

// Microsoft Entra allow-list — chỉ các host này được phép làm external IdP endpoint.
// Leading dot ensures subdomain boundary: "evil-microsoftonline.com" cannot match.
var allowedExternalIdpHosts = []string{
	".microsoftonline.com",
	".microsoftonline.us",
	".microsoftonline.cn",
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// StartKiroSsoLogin khởi tạo flow Kiro SSO External IdP.
// loginHint có thể để trống — user sẽ nhập email ở Kiro portal.
// Leg-1 URL gửi đúng 5 params như zsec: state, code_challenge, code_challenge_method,
// redirect_uri (root path), redirect_from. KHÔNG gửi client_id, response_type, scope.
// redirectBase, when non-empty, is the externally reachable base URL (scheme+host
// [+port], no path) the IdP must redirect back to — e.g. "https://azr.hian.software".
// Pass "" for the pure local flow (falls back to http://localhost:<port>).
func StartKiroSsoLogin(loginHint, redirectBase string) (sessionID, authorizeURL string, loopbackPort int, err error) {
	// 0. Một user chỉ có 1 flow login active tại một thời điểm. Mỗi session pending
	// giữ 1 port loopback trong tối đa 10 phút; nếu user bấm "start" nhiều lần (login
	// dở dang, đổi tài khoản, refresh trang) thì các session cũ leak port, và chỉ có
	// 10 port → cạn nhanh, gây lỗi "tất cả port loopback đều bận". Hủy mọi session cũ
	// ngay khi bắt đầu flow mới để trả port về tức thì.
	cancelAllPendingKiroSsoSessions()

	// 1. Tạo PKCE pair cho Leg-1 (Kiro portal)
	codeVerifier := generateCodeVerifier()
	codeChallenge := generateCodeChallenge(codeVerifier)
	state := uuid.New().String()

	// 2. Bind loopback server trên tập port chính thức của Kiro
	listener, loopbackPort, err := bindKiroLoopback()
	if err != nil {
		return "", "", 0, fmt.Errorf("không thể bind loopback server: %w", err)
	}

	// 3. Leg-1 redirect_uri = root path (giống zsec SocialRedirectURI).
	// Dùng redirectBase khi proxy được truy cập qua custom domain / reverse proxy,
	// nếu không thì fallback localhost:<port> cho flow local thuần.
	redirectBase = strings.TrimRight(strings.TrimSpace(redirectBase), "/")
	redirectURI := redirectBase
	if redirectURI == "" {
		redirectURI = fmt.Sprintf("http://localhost:%d", loopbackPort)
	}

	// 4. Xây dựng Kiro portal authorize URL (Leg-1) — đúng 5 params
	params := url.Values{}
	params.Set("state", state)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("redirect_uri", redirectURI)
	params.Set("redirect_from", kiroRedirectFrom)

	authorizeURL = fmt.Sprintf("%s?%s", kiroPortalSignInURL, params.Encode())

	// 5. Tạo session
	sessionID = uuid.New().String()
	session := &KiroSsoSession{
		ID:            sessionID,
		State:         state,
		CodeVerifier:  codeVerifier,
		CodeChallenge: codeChallenge,
		LoopbackPort:  loopbackPort,
		LoopbackReady: make(chan struct{}),
		LoginHint:     loginHint,
		RedirectBase:  redirectBase,
		ResultCh:      make(chan KiroSsoResult, 1),
		ExpiresAt:     time.Now().Add(10 * time.Minute),
	}

	kiroSsoSessionsMu.Lock()
	kiroSsoSessions[sessionID] = session
	kiroSsoSessionsMu.Unlock()

	// 6. Khởi động loopback HTTP server với catch-all handler (zsec pattern)
	mux := http.NewServeMux()
	mux.HandleFunc("/", session.handleLoopback)

	loopbackHost := "127.0.0.1"
	if v := strings.TrimSpace(os.Getenv("LOOPBACK_HOST")); v != "" {
		loopbackHost = v
	}
	session.LoopbackServer = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", loopbackHost, loopbackPort),
		Handler: mux,
	}

	go func() {
		close(session.LoopbackReady)
		serveLoopback(session, listener, loopbackPort)
	}()

	// 7. Background cleanup goroutine
	go cleanupExpiredKiroSsoSessions()

	return sessionID, authorizeURL, loopbackPort, nil
}

// PollKiroSsoLogin kiểm tra trạng thái flow Kiro SSO.
// Trả về status "pending" hoặc "completed".
func PollKiroSsoLogin(sessionID string) (status string, result *KiroSsoResult, err error) {
	kiroSsoSessionsMu.RLock()
	session, ok := kiroSsoSessions[sessionID]
	kiroSsoSessionsMu.RUnlock()

	if !ok {
		return "", nil, fmt.Errorf("phiên không tồn tại hoặc đã hết hạn")
	}

	if time.Now().After(session.ExpiresAt) {
		kiroSsoSessionsMu.Lock()
		delete(kiroSsoSessions, sessionID)
		kiroSsoSessionsMu.Unlock()
		shutdownLoopbackServer(session)
		return "", nil, fmt.Errorf("phiên đã hết hạn")
	}

	select {
	case res := <-session.ResultCh:
		kiroSsoSessionsMu.Lock()
		delete(kiroSsoSessions, sessionID)
		kiroSsoSessionsMu.Unlock()
		shutdownLoopbackServer(session)

		if res.Err != nil {
			return "", nil, res.Err
		}

		result = &KiroSsoResult{
			AccessToken:  res.AccessToken,
			RefreshToken: res.RefreshToken,
			ExpiresIn:    res.ExpiresIn,
			IssuerURL:    res.IssuerURL,
			IdPClientID:  res.IdPClientID,
			Scopes:       res.Scopes,
			LoginHint:    res.LoginHint,
			UserEmail:    res.UserEmail,
		}
		return "completed", result, nil
	default:
		return "pending", nil, nil
	}
}

// CancelKiroSsoLogin hủy session và shutdown loopback server.
func CancelKiroSsoLogin(sessionID string) {
	kiroSsoSessionsMu.Lock()
	session, ok := kiroSsoSessions[sessionID]
	if ok {
		delete(kiroSsoSessions, sessionID)
	}
	kiroSsoSessionsMu.Unlock()

	if ok {
		shutdownLoopbackServer(session)
	}
}

// GetKiroSsoSession trả về session info (dùng cho test/debug).
func GetKiroSsoSession(sessionID string) *KiroSsoSession {
	kiroSsoSessionsMu.RLock()
	defer kiroSsoSessionsMu.RUnlock()
	return kiroSsoSessions[sessionID]
}

// validateExternalIdpEndpoints gates the SSRF allow-list check on the token-refresh
// path. Always true in production; refresh-path unit tests that drive local httptest
// servers set it to false. The write boundaries (credential import, local-cache scan,
// and the login flow) validate unconditionally, so this refresh-path check is
// defense-in-depth against a hand-tampered config.json.
var validateExternalIdpEndpoints = true

// ValidateExternalIdpURL is the exported entry point for callers outside this package
// (e.g. credential import) to enforce the external-IdP allow-list on untrusted issuer /
// endpoint URLs before they are stored or used to make a request.
func ValidateExternalIdpURL(rawURL string) error {
	return validateExternalIdpURL(rawURL)
}

// RefreshExternalIdpToken làm mới access token qua IdP token endpoint.
// Được gọi từ oidc.go khi AuthMethod == "external_idp".
func RefreshExternalIdpToken(refreshToken, issuerURL, tokenEndpoint, clientID, scopes string, httpClient *http.Client) (string, string, int64, string, error) {
	if issuerURL == "" {
		return "", "", 0, "", fmt.Errorf("external_idp refresh requires issuerUrl")
	}
	if clientID == "" {
		return "", "", 0, "", fmt.Errorf("external_idp refresh requires idpClientId")
	}

	// SSRF guard: the issuer is used to run OIDC discovery (an HTTP GET) below, so it
	// must be allow-listed BEFORE any request is made. Mirrors the login-path check.
	if validateExternalIdpEndpoints {
		if err := validateExternalIdpURL(issuerURL); err != nil {
			return "", "", 0, "", fmt.Errorf("external_idp refresh: invalid issuer_url: %w", err)
		}
	}

	// Dùng cached token endpoint nếu có, nếu không thì OIDC discovery
	if tokenEndpoint == "" {
		var err error
		tokenEndpoint, err = resolveExternalIdpTokenEndpoint(issuerURL, httpClient)
		if err != nil {
			return "", "", 0, "", err
		}
	}

	// SSRF guard: whether the token endpoint came from a cached (possibly tampered)
	// value or from discovery, validate it before POSTing credentials to it.
	if validateExternalIdpEndpoints {
		if err := validateExternalIdpURL(tokenEndpoint); err != nil {
			return "", "", 0, "", fmt.Errorf("external_idp refresh: invalid token endpoint: %w", err)
		}
	}

	// POST refresh_token grant (form-encoded, giống zsec postExternalIdpToken)
	payload := url.Values{}
	payload.Set("client_id", clientID)
	payload.Set("grant_type", "refresh_token")
	payload.Set("refresh_token", refreshToken)
	if scopes != "" {
		payload.Set("scope", scopes)
	}

	req, _ := http.NewRequest("POST", tokenEndpoint, strings.NewReader(payload.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := noRedirectClient(httpClient)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", fmt.Errorf("token refresh failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", "", 0, "", fmt.Errorf("token refresh failed: HTTP %d — %s", resp.StatusCode, readErrorBody(resp.Body))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		ProfileArn   string `json:"profileArn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, "", err
	}

	newRefreshToken := result.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = refreshToken
	}

	expiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	return result.AccessToken, newRefreshToken, expiresAt, result.ProfileArn, nil
}

// ---------------------------------------------------------------------------
// Loopback HTTP Handler — single catch-all dispatcher (zsec pattern)
// ---------------------------------------------------------------------------

// handleLoopback là catch-all handler cho loopback server.
// Dispatch dựa trên path và query params:
//   - /oauth/callback → enterprise Leg-2 (IdP code callback)
//   - root path + query có login_option=external_idp hoặc issuer_url → enterprise Leg-1
//   - otherwise → social/cognito fallback (stray hit = 204)
func (s *KiroSsoSession) handleLoopback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	path := r.URL.Path

	// Enterprise Leg-2: IdP code callback tại /oauth/callback
	if path == "/oauth/callback" {
		s.handleOAuthCallback(w, r)
		return
	}

	// Enterprise Leg-1: Kiro portal external IdP descriptor
	// Điều kiện: login_option=external_idp HOẶC issuer_url không rỗng
	loginOption := query.Get("login_option")
	issuerURL := query.Get("issuer_url")
	if strings.EqualFold(loginOption, "external_idp") || issuerURL != "" {
		reqState := query.Get("state")
		logger.Infof("[KiroSSO] Leg-1 hit path=%s state_match=%t", path, reqState == s.State)

		// State phải khớp Leg-1 — chặn stray hit / CSRF.
		if reqState == "" || reqState != s.State {
			writeSSOErrorPage(w, "Trạng thái không khớp — có thể là tấn công CSRF.")
			s.pushError(fmt.Errorf("state mismatch on external IdP descriptor"))
			return
		}

		// Idempotent: tính Microsoft authorize URL đúng MỘT lần (dưới mutex), cache lại.
		// Mọi hit sau với cùng state — kể cả tab khác hoặc trình duyệt ẩn danh copy URL —
		// đều nhận lại đúng URL đó (chung PKCE pair), nên browser nào hoàn tất login cũng
		// đổi được token. Nếu tính fail (vd OIDC discovery lỗi) thì KHÔNG khóa session —
		// hit sau sẽ thử lại.
		authorizeURL, err := s.resolveIdpAuthorizeURL(query)
		if err != nil {
			logger.Warnf("[KiroSSO] Leg-1 resolve authorize URL failed: %v", err)
			writeSSOErrorPage(w, fmt.Sprintf("Không thể khởi tạo đăng nhập IdP: %v", err))
			s.pushError(err)
			return
		}
		w.Header().Set("Location", authorizeURL)
		w.WriteHeader(http.StatusFound)
		return
	}

	// Social/Cognito fallback hoặc stray hit → 204 No Content
	logger.Debugf("[KiroSSO] Loopback stray hit path=%s → 204", path)
	w.WriteHeader(http.StatusNoContent)
}

// resolveIdpAuthorizeURL xử lý descriptor từ Kiro portal và trả về Microsoft Entra
// authorize URL (Leg-2). Idempotent: tính đúng MỘT lần rồi cache; các lần gọi sau với
// cùng state trả lại URL đã cache (chung PKCE pair) — cho phép user copy URL mở ở
// trình duyệt/profile khác mà vẫn hoàn tất được login. Caller đã verify state khớp Leg-1.
//
// Query params từ portal: login_option=external_idp, issuer_url, client_id, scopes, login_hint.
func (s *KiroSsoSession) resolveIdpAuthorizeURL(query url.Values) (string, error) {
	// Fast path: đã tính rồi → trả lại URL cache (idempotent cho hit lặp / ẩn danh).
	s.loopbackMu.Lock()
	if s.idpAuthorizeURL != "" {
		cached := s.idpAuthorizeURL
		s.loopbackMu.Unlock()
		return cached, nil
	}
	s.loopbackMu.Unlock()

	// Check for error from portal
	if errParam := query.Get("error"); errParam != "" {
		errDesc := query.Get("error_description")
		return "", fmt.Errorf("kiro portal error: %s — %s", errParam, errDesc)
	}

	issuerURL := query.Get("issuer_url")
	clientID := query.Get("client_id")
	scopes := query.Get("scopes")
	loginHint := query.Get("login_hint")

	if clientID == "" {
		return "", fmt.Errorf("missing client_id from portal")
	}
	if issuerURL == "" {
		return "", fmt.Errorf("missing issuer_url from portal")
	}

	// Validate issuer URL against Microsoft allow-list
	if err := validateExternalIdpURL(issuerURL); err != nil {
		return "", fmt.Errorf("invalid issuer_url: %w", err)
	}

	// OIDC discovery để lấy authorization và token endpoints
	authEndpoint, tokenEndpoint, err := discoverOIDCEndpoints(issuerURL, nil)
	if err != nil {
		return "", fmt.Errorf("OIDC discovery failed: %w", err)
	}

	// Validate discovered endpoints against allow-list
	if err := validateExternalIdpURL(authEndpoint); err != nil {
		return "", fmt.Errorf("invalid auth endpoint: %w", err)
	}
	if err := validateExternalIdpURL(tokenEndpoint); err != nil {
		return "", fmt.Errorf("invalid token endpoint: %w", err)
	}

	// Tạo PKCE pair mới cho Leg-2 (96-byte verifier như zsec randomURLSafe(96))
	verifierBytes := make([]byte, 96)
	if _, err := io.ReadFull(rand.Reader, verifierBytes); err != nil {
		return "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	idpCodeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	idpCodeChallenge := generateCodeChallenge(idpCodeVerifier)
	idpState := uuid.New().String()

	// Xây dựng Microsoft Entra authorize URL (Leg-2)
	// redirect_uri = <base>/oauth/callback (zsec OAuthCallbackPath)
	redirectURI := s.redirectBaseURL() + "/oauth/callback"

	idpParams := url.Values{}
	idpParams.Set("client_id", clientID)
	idpParams.Set("response_type", "code")
	idpParams.Set("redirect_uri", redirectURI)
	idpParams.Set("scope", scopes)
	idpParams.Set("code_challenge", idpCodeChallenge)
	idpParams.Set("code_challenge_method", "S256")
	idpParams.Set("response_mode", "query")
	idpParams.Set("state", idpState)
	if loginHint != "" {
		idpParams.Set("login_hint", loginHint)
	}

	idpAuthorizeURL := fmt.Sprintf("%s?%s", authEndpoint, idpParams.Encode())

	// Lưu IdP metadata + cache URL dưới mutex. Double-check: nếu một goroutine khác
	// (browser prefetch / hit song song) đã cache trước thì dùng URL của nó để mọi
	// browser chia sẻ đúng một PKCE pair.
	s.loopbackMu.Lock()
	if s.idpAuthorizeURL != "" {
		cached := s.idpAuthorizeURL
		s.loopbackMu.Unlock()
		return cached, nil
	}
	if loginHint != "" {
		s.LoginHint = loginHint
	}
	s.IssuerURL = issuerURL
	s.IdPClientID = clientID
	s.IdPScopes = scopes
	s.IdPAuthEndpoint = authEndpoint
	s.IdPTokenEndpoint = tokenEndpoint
	s.IdPCodeVerifier = idpCodeVerifier
	s.IdPState = idpState
	s.idpAuthorizeURL = idpAuthorizeURL
	s.loopbackMu.Unlock()

	return idpAuthorizeURL, nil
}

// handleOAuthCallback xử lý redirect từ external IdP sau khi user xác thực.
// Path: /oauth/callback?code=...&state=...
func (s *KiroSsoSession) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	// Check for error from IdP
	if errParam := query.Get("error"); errParam != "" {
		errDesc := query.Get("error_description")
		writeSSOErrorPage(w, fmt.Sprintf("Đăng nhập thất bại: %s — %s", errParam, errDesc))
		s.pushError(fmt.Errorf("idp error: %s — %s", errParam, errDesc))
		return
	}

	// Validate state (Leg-2 CSRF)
	// zsec: state empty or mismatch → 204 (don't consume one-shot)
	state := query.Get("state")
	logger.Infof("[KiroSSO] Leg-2 callback hit state_present=%t state_match=%t has_code=%t",
		state != "", state == s.IdPState, query.Get("code") != "")
	if state == "" || state != s.IdPState {
		logger.Warnf("[KiroSSO] Leg-2 state mismatch → 204 (got=%q want=%q)", state, s.IdPState)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	code := query.Get("code")
	if code == "" {
		writeSSOErrorPage(w, "Không nhận được authorization code từ IdP.")
		s.pushError(fmt.Errorf("missing authorization code"))
		return
	}

	// Đổi code lấy token từ IdP
	redirectURI := s.redirectBaseURL() + "/oauth/callback"

	if s.IdPTokenEndpoint == "" {
		writeSSOErrorPage(w, "Thiếu thông tin IdP — vui lòng thử lại.")
		s.pushError(fmt.Errorf("missing IdP token endpoint"))
		return
	}

	tokenResult, err := exchangeExternalIdpCode(
		s.IdPTokenEndpoint,
		s.IdPClientID,
		code,
		s.IdPCodeVerifier,
		redirectURI,
	)
	if err != nil {
		writeSSOErrorPage(w, fmt.Sprintf("Không thể đổi code lấy token: %v", err))
		s.pushError(fmt.Errorf("token exchange failed: %w", err))
		return
	}

	email := s.LoginHint

	// Push kết quả
	select {
	case s.ResultCh <- KiroSsoResult{
		AccessToken:  tokenResult.AccessToken,
		RefreshToken: tokenResult.RefreshToken,
		ExpiresIn:    tokenResult.ExpiresIn,
		IssuerURL:    s.IssuerURL,
		IdPClientID:  s.IdPClientID,
		Scopes:       s.IdPScopes,
		LoginHint:    s.LoginHint,
		UserEmail:    email,
				IdPTokenEndpoint: s.IdPTokenEndpoint,
	}:
	default:
	}

	writeSSOSuccessPage(w)

	// Shutdown loopback server sau short delay (để response kịp gửi)
	go func() {
		time.Sleep(2 * time.Second)
		shutdownLoopbackServer(s)
	}()
}

// pushError gửi lỗi vào ResultCh (non-blocking).
func (s *KiroSsoSession) pushError(err error) {
	select {
	case s.ResultCh <- KiroSsoResult{Err: err}:
	default:
	}
}

// ---------------------------------------------------------------------------
// Internal: PKCE & Token Exchange
// ---------------------------------------------------------------------------

// exchangeExternalIdpCode đổi authorization code lấy token từ Microsoft Entra IdP.
// POST form-encoded đến token endpoint. Response dùng snake_case JSON.
func exchangeExternalIdpCode(tokenEndpoint, clientID, code, codeVerifier, redirectURI string) (*KiroSsoResult, error) {
	payload := url.Values{}
	payload.Set("client_id", clientID)
	payload.Set("grant_type", "authorization_code")
	payload.Set("code", code)
	payload.Set("redirect_uri", redirectURI)
	payload.Set("code_verifier", codeVerifier)

	req, _ := http.NewRequest("POST", tokenEndpoint, strings.NewReader(payload.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := externalIdpHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parse token response failed: %w", err)
	}

	if result.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in response")
	}

	return &KiroSsoResult{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    result.ExpiresIn,
	}, nil
}

// ---------------------------------------------------------------------------
// Internal: OIDC Discovery
// ---------------------------------------------------------------------------

// oidcDiscoveryResponse là cấu trúc .well-known/openid-configuration.
type oidcDiscoveryResponse struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	Issuer                string `json:"issuer"`
}

// discoverOIDCEndpoints fetch OIDC discovery document từ issuer URL.
// Không follow redirect (CheckRedirect → ErrUseLastResponse) — zsec pattern.
func discoverOIDCEndpoints(issuerURL string, client *http.Client) (authEndpoint, tokenEndpoint string, err error) {
	discoveryURL := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"

	resp, err := noRedirectClient(client).Get(discoveryURL)
	if err != nil {
		return "", "", fmt.Errorf("fetch discovery failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("discovery returned %d", resp.StatusCode)
	}

	var discovery oidcDiscoveryResponse
	if err := json.NewDecoder(resp.Body).Decode(&discovery); err != nil {
		return "", "", fmt.Errorf("parse discovery failed: %w", err)
	}

	if discovery.AuthorizationEndpoint == "" {
		return "", "", fmt.Errorf("missing authorization_endpoint in discovery")
	}
	if discovery.TokenEndpoint == "" {
		return "", "", fmt.Errorf("missing token_endpoint in discovery")
	}

	return discovery.AuthorizationEndpoint, discovery.TokenEndpoint, nil
}

// resolveExternalIdpTokenEndpoint lấy token endpoint từ issuer URL qua OIDC discovery.
func resolveExternalIdpTokenEndpoint(issuerURL string, client *http.Client) (string, error) {
	if externalIdpTokenURLFn != nil {
		return externalIdpTokenURLFn(issuerURL, client)
	}
	_, tokenEndpoint, err := discoverOIDCEndpoints(issuerURL, client)
	return tokenEndpoint, err
}

// ---------------------------------------------------------------------------
// Internal: Security — Microsoft Allow-List
// ---------------------------------------------------------------------------

// validateExternalIdpURL kiểm tra URL có hợp lệ cho external IdP:
//   - Phải là HTTPS
//   - Không được là IP literal
//   - Host phải thuộc allow-list (Microsoft Entra domains)
//
// (zsec validateExternalIdpEndpoint)
func validateExternalIdpURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("URL không hợp lệ: %w", err)
	}

	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("chỉ hỗ trợ HTTPS, nhận được %s", u.Scheme)
	}

	hostname := u.Hostname()
	if hostname == "" {
		return fmt.Errorf("thiếu hostname trong URL")
	}

	// Từ chối IP literals (IPv4 và IPv6)
	if ip := net.ParseIP(hostname); ip != nil {
		return fmt.Errorf("không cho phép IP literal: %s", hostname)
	}

	// Kiểm tra allow-list (leading dot = subdomain boundary)
	hostnameLower := strings.ToLower(hostname)
	for _, allowed := range allowedExternalIdpHosts {
		if hostnameLower == strings.TrimPrefix(allowed, ".") || strings.HasSuffix(hostnameLower, allowed) {
			return nil
		}
	}

	return fmt.Errorf("host %s không nằm trong danh sách cho phép", hostname)
}

// externalIdpHTTPClient tạo HTTP client không follow redirect.
// Ngăn chặn SSRF qua redirect đến internal/link-local targets (zsec pattern).
func externalIdpHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// noRedirectClient returns a client that does not follow redirects (SSRF guard,
// zsec pattern). When base is nil it builds a fresh non-proxied client; when a
// proxy-aware base is supplied it reuses base's Transport but enforces the
// no-redirect policy.
func noRedirectClient(base *http.Client) *http.Client {
	if base == nil {
		return externalIdpHTTPClient()
	}
	c := *base
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c
}

// ---------------------------------------------------------------------------
// Internal: Loopback HTML Pages
// ---------------------------------------------------------------------------

// writeSSOErrorPage render trang HTML báo lỗi cho browser.
func writeSSOErrorPage(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	io.WriteString(w, `<!DOCTYPE html>
<html lang="vi">
<head><meta charset="UTF-8"><title>Kiro SSO — Lỗi</title>
<style>body{font-family:system-ui;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0;background:#fafafa}.card{background:#fff;border-radius:12px;padding:40px;text-align:center;box-shadow:0 2px 12px rgba(0,0,0,.08);max-width:420px}.icon{font-size:48px;margin-bottom:16px}h2{margin:0 0 8px;color:#d32f2f}p{color:#666;font-size:14px;margin:0}</style>
</head>
<body><div class="card"><div class="icon">&#10060;</div><h2>Đăng nhập thất bại</h2><p>`+escapeHTML(message)+`</p></div></body></html>`)
}

// writeSSOSuccessPage render trang HTML thành công cho browser.
func writeSSOSuccessPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	io.WriteString(w, `<!DOCTYPE html>
<html lang="vi">
<head><meta charset="UTF-8"><title>Kiro SSO — Thành công</title>
<style>body{font-family:system-ui;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0;background:#fafafa}.card{background:#fff;border-radius:12px;padding:40px;text-align:center;box-shadow:0 2px 12px rgba(0,0,0,.08);max-width:420px}.icon{font-size:48px;margin-bottom:16px}h2{margin:0 0 8px;color:#2e7d32}p{color:#666;font-size:14px;margin:4px 0}</style>
</head>
<body><div class="card"><div class="icon">&#9989;</div><h2>Đăng nhập thành công!</h2><p>Bạn có thể đóng cửa sổ này.</p><p style="font-size:12px;margin-top:12px">Tài khoản đã được thêm vào Kiro-Go.</p></div></body></html>`)
}

// escapeHTML escape các ký tự HTML cơ bản.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// ---------------------------------------------------------------------------
// Internal: Session Cleanup
// ---------------------------------------------------------------------------

// shutdownLoopbackServer shutdown loopback server của session (nếu có).
func shutdownLoopbackServer(s *KiroSsoSession) {
	if s.LoopbackServer != nil {
		s.LoopbackServer.Close()
	}
}

// cancelAllPendingKiroSsoSessions hủy TẤT CẢ session SSO đang tồn tại và giải phóng
// loopback port của chúng ngay lập tức. Gọi khi bắt đầu một flow login mới: một user
// chỉ có 1 flow active, nên session cũ (login dở dang, đổi tài khoản, refresh trang)
// chỉ tổ leak port. Khác với cleanupExpiredKiroSsoSessions (chỉ dọn session đã hết hạn),
// hàm này dọn cả session chưa hết hạn.
func cancelAllPendingKiroSsoSessions() {
	kiroSsoSessionsMu.Lock()
	stale := make([]*KiroSsoSession, 0, len(kiroSsoSessions))
	for id, s := range kiroSsoSessions {
		stale = append(stale, s)
		delete(kiroSsoSessions, id)
	}
	kiroSsoSessionsMu.Unlock()

	for _, s := range stale {
		shutdownLoopbackServer(s)
	}
}

// cleanupExpiredKiroSsoSessions dọn dẹp session hết hạn.
func cleanupExpiredKiroSsoSessions() {
	kiroSsoSessionsMu.Lock()
	defer kiroSsoSessionsMu.Unlock()

	now := time.Now()
	for id, s := range kiroSsoSessions {
		if now.After(s.ExpiresAt) {
			shutdownLoopbackServer(s)
			delete(kiroSsoSessions, id)
		}
	}
}

// ---------------------------------------------------------------------------
// Token Refresh helpers
// ---------------------------------------------------------------------------

// init registers the default external IdP token URL resolver.
func init() {
	externalIdpTokenURLFn = func(issuerURL string, client *http.Client) (string, error) {
		_, tokenEndpoint, err := discoverOIDCEndpoints(issuerURL, client)
		return tokenEndpoint, err
	}
}
