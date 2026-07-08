// Package main provides the entry point for Kiro API Proxy.
//
// Kiro API Proxy is a reverse proxy service that translates Kiro API requests
// into OpenAI and Anthropic (Claude) compatible formats. Key features include:
//   - Multi-account pool with round-robin load balancing
//   - Automatic OAuth token refresh
//   - Streaming response support for real-time AI interactions
//   - Admin panel for account and configuration management
//
// The service exposes the following endpoints:
//   - /v1/messages - Claude API compatible endpoint
//   - /v1/chat/completions - OpenAI API compatible endpoint
//   - /admin - Web-based administration panel
package main

import (
	"context"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"kiro-go/proxy"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	// 配置文件路径，支持环境变量覆盖
	configPath := "data/config.json"
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}

	// 确保数据目录存在
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// 加载配置
	if err := config.Init(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize log level: LOG_LEVEL env var takes priority over config, defaulting to "info".
	logger.Init(config.GetLogLevel())

	// 环境变量覆盖密码
	if envPassword := os.Getenv("ADMIN_PASSWORD"); envPassword != "" {
		config.SetPassword(envPassword)
	}

	// Security guard: refuse to boot a publicly-reachable instance that still uses the
	// built-in default admin password. A public VPS that forgets to set ADMIN_PASSWORD
	// would otherwise expose full admin access (account tokens, key creation) to anyone.
	// Loopback-only binds are allowed for local development.
	if config.GetPassword() == "changeme" && !isLoopbackHost(config.GetHost()) {
		log.Fatalf("Refusing to start: admin password is still the default on a non-loopback host (%s). "+
			"Set the ADMIN_PASSWORD environment variable to a strong secret, or bind to 127.0.0.1.", config.GetHost())
	}

	// 初始化账号池
	pool.GetPool()

	// 创建 HTTP 处理器（包含后台刷新任务）
	handler := proxy.NewHandler()

	// 启动服务器
	addr := fmt.Sprintf("%s:%d", config.GetHost(), config.GetPort())
	logger.Infof("Kiro-Go starting on http://%s (log level: %s)", addr, logger.LevelName(logger.GetLevel()))
	logger.Infof("Admin panel: http://%s/admin", addr)
	logger.Infof("Claude API: http://%s/v1/messages", addr)
	logger.Infof("OpenAI API: http://%s/v1/chat/completions", addr)

	// WriteTimeout intentionally 0: SSE streams can run for minutes while the
	// upstream model produces tokens. ReadHeaderTimeout + ReadTimeout still
	// guard against slowloris-style header/body stalls.
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB — cap header size to blunt header-flood / slowloris.
	}

	// Graceful shutdown: on SIGINT/SIGTERM stop accepting new connections, let
	// in-flight requests drain, stop background goroutines, and flush any pending
	// hot-path config changes (usage/stats) so nothing accumulated since the last
	// 30s tick is lost — e.g. on `docker-compose down`.
	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		logger.Fatalf("Server failed: %v", err)
	case sig := <-stop:
		logger.Infof("Received %s, shutting down gracefully...", sig)
	}

	// Stop background goroutines and flush stats/usage before the process exits.
	handler.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Warnf("Graceful shutdown timed out: %v", err)
	}
	logger.Infof("Shutdown complete")
}

// isLoopbackHost reports whether the configured bind host is loopback-only, in which
// case the admin panel is not reachable from the network and the default-password
// startup guard can be safely skipped.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
