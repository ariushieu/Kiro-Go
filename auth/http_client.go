// Package auth 提供认证相关功能的 HTTP 客户端
package auth

import (
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// maxErrorBodyBytes caps how much of a non-2xx upstream body we fold into an error
// string. OAuth/OIDC error bodies can carry tokens, session ids, or internal
// hostnames that then propagate to the admin API / SSO error pages; a full dump is
// both a leak risk and a log-bloat risk. The truncated prefix is enough to identify
// the error class (e.g. `{"error":"invalid_grant"}`); the full body is only logged
// at debug level by callers that need it.
const maxErrorBodyBytes = 512

// readErrorBody reads at most maxErrorBodyBytes from an upstream error response and
// returns it as a string, appending an ellipsis marker when the body was truncated.
// It never returns an error — a body that cannot be read yields an empty string.
func readErrorBody(body io.Reader) string {
	if body == nil {
		return ""
	}
	buf, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes+1))
	if len(buf) > maxErrorBodyBytes {
		return string(buf[:maxErrorBodyBytes]) + "…(truncated)"
	}
	return string(buf)
}

// 全局 HTTP 客户端存储，支持运行时代理重配置
var httpClientStore atomic.Pointer[http.Client]

// authProxyClientCache caches per-proxy auth HTTP clients.
var authProxyClientCache sync.Map

// httpClient 返回当前全局 auth HTTP 客户端
func httpClient() *http.Client {
	return httpClientStore.Load()
}

func init() {
	InitHttpClient("")
}

// GetAuthClientForProxy returns an auth HTTP client for the given proxy URL.
// If proxyURL is empty, returns the global auth HTTP client.
func GetAuthClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return httpClient()
	}
	if cached, ok := authProxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildAuthTransport(proxyURL),
	}
	authProxyClientCache.Store(proxyURL, client)
	return client
}

// buildAuthTransport 构建带可选代理的 Transport
func buildAuthTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

// InitHttpClient 初始化（或重新初始化）auth 模块的全局 HTTP 客户端
func InitHttpClient(proxyURL string) {
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildAuthTransport(proxyURL),
	}
	httpClientStore.Store(client)
}
