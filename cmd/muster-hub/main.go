package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/feishu"
	"github.com/baiyucraft/agent-muster/internal/hub/httpapi"
	"github.com/baiyucraft/agent-muster/internal/hub/im"
	"github.com/baiyucraft/agent-muster/internal/hub/postgres"
	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		if err := runAdmin(os.Args[2:], os.Stdin, os.Stderr); err != nil {
			log.Fatal(err)
		}
		return
	}
	address := flag.String("listen", "127.0.0.1:8080", "HTTP 监听地址")
	webDir := flag.String("web-dir", "", "Web 构建产物目录")
	version := flag.Bool("version", false, "显示版本")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("未知子命令，请使用 muster-hub admin -h 查看管理员命令")
	}
	if *version {
		fmt.Println("muster-hub dev")
		return
	}
	if err := serve(*address, *webDir); err != nil {
		log.Fatal(err)
	}
}

func serve(address, webDir string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var web http.Handler
	if webDir != "" {
		files := os.DirFS(webDir)
		index, err := files.Open("index.html")
		if err != nil {
			return errors.New("无法读取 Web 构建产物 index.html")
		}
		index.Close()
		web = http.FileServerFS(files)
	}

	var store registry.Store
	var workStore workspace.Store
	var imStore im.Store
	var auth httpapi.AdminAuthOptions
	databaseURL := os.Getenv("MUSTER_DATABASE_URL")
	adminToken := os.Getenv("MUSTER_ADMIN_TOKEN")
	pluginToken := os.Getenv("MUSTER_DSH_PLUGIN_TOKEN")
	if adminToken != "" && (databaseURL == "" || !httpapi.ValidAdminToken(adminToken)) {
		return errors.New("启用管理员 API 令牌需要数据库连接和有效的 MUSTER_ADMIN_TOKEN")
	}
	if value := os.Getenv("MUSTER_ADMIN_COOKIE_SECURE"); value != "" {
		secure, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("MUSTER_ADMIN_COOKIE_SECURE 须为 true 或 false")
		}
		auth.SecureCookie = secure
	}
	if pluginToken != "" && (!httpapi.ValidAdminToken(pluginToken) || pluginToken == adminToken || databaseURL == "") {
		return errors.New("启用工作台需要数据库连接和独立的 MUSTER_DSH_PLUGIN_TOKEN")
	}
	recipient, feishuSecret, err := feishuConfig(pluginToken != "")
	if err != nil {
		return err
	}
	if databaseURL != "" {
		initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		database, err := postgres.OpenInventory(initCtx, databaseURL)
		cancel()
		if err != nil {
			return fmt.Errorf("无法连接或初始化设备登记数据库，请核对连接配置、网络与建表权限 (%T)", err)
		}
		defer database.Close()
		store = database
		workStore = database
		auth.Store = database
		if feishuSecret != "" {
			setup, finish := context.WithTimeout(ctx, 10*time.Second)
			err := database.EnableApprovalNotifications(setup, recipient)
			finish()
			if err != nil {
				return fmt.Errorf("无法初始化飞书请示通知 (%T)", err)
			}
			imStore = database
		}
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("监听 HTTP 地址失败: %w", err)
	}
	server := &http.Server{
		Handler: httpapi.NewHandler(store, adminToken, web, httpapi.WorkspaceOptions{
			Store: workStore, PluginToken: pluginToken, AdminAuth: auth,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if imStore != nil {
		notifications, cancel := context.WithCancel(ctx)
		workerDone := make(chan struct{})
		go func() {
			defer close(workerDone)
			im.Run(notifications, imStore, recipient, feishu.New(recipient.AppID, feishuSecret))
		}()
		defer func() {
			cancel()
			<-workerDone
		}()
	}
	log.Printf("muster-hub 正在监听 http://%s", listener.Addr())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			server.Close()
		}
	}()
	err = server.Serve(listener)
	stop()
	<-stopped
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// feishuConfig 读取允许发起和答复请示的固定私聊绑定。三项均空时停用渠道。
func feishuConfig(workspaceEnabled bool) (im.Recipient, string, error) {
	recipient := im.Recipient{
		AppID:  os.Getenv("MUSTER_FEISHU_APP_ID"),
		OpenID: os.Getenv("MUSTER_FEISHU_OPEN_ID"),
	}
	secret := os.Getenv("MUSTER_FEISHU_APP_SECRET")
	if recipient.AppID == "" && recipient.OpenID == "" && secret == "" {
		return recipient, "", nil
	}
	if !workspaceEnabled || recipient.AppID == "" || recipient.OpenID == "" || secret == "" {
		return im.Recipient{}, "", errors.New("启用飞书请示需要工作台及完整的 MUSTER_FEISHU_APP_ID、MUSTER_FEISHU_APP_SECRET、MUSTER_FEISHU_OPEN_ID")
	}
	return recipient, secret, nil
}
