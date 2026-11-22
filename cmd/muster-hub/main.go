package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/httpapi"
	"github.com/baiyucraft/agent-muster/internal/hub/postgres"
	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func main() {
	address := flag.String("listen", "127.0.0.1:8080", "HTTP 监听地址")
	webDir := flag.String("web-dir", "", "Web 构建产物目录")
	version := flag.Bool("version", false, "显示版本")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("muster-hub 仅接受命令行选项")
	}
	if *version {
		fmt.Println("muster-hub dev")
		return
	}

	var web http.Handler
	if *webDir != "" {
		files := os.DirFS(*webDir)
		index, err := files.Open("index.html")
		if err != nil {
			log.Fatal("无法读取 Web 构建产物 index.html")
		}
		index.Close()
		web = http.FileServerFS(files)
	}

	var store registry.Store
	var workStore workspace.Store
	databaseURL := os.Getenv("MUSTER_DATABASE_URL")
	adminToken := os.Getenv("MUSTER_ADMIN_TOKEN")
	pluginToken := os.Getenv("MUSTER_DSH_PLUGIN_TOKEN")
	if pluginToken != "" && (!httpapi.ValidAdminToken(pluginToken) || pluginToken == adminToken || databaseURL == "") {
		log.Fatal("启用工作台需要数据库连接和独立的 MUSTER_DSH_PLUGIN_TOKEN")
	}
	if databaseURL != "" || adminToken != "" {
		if databaseURL == "" || !httpapi.ValidAdminToken(adminToken) {
			log.Fatal("启用设备登记需要 MUSTER_DATABASE_URL 和有效的 MUSTER_ADMIN_TOKEN")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		database, err := postgres.OpenInventory(ctx, databaseURL)
		cancel()
		if err != nil {
			log.Fatalf("无法连接或初始化设备登记数据库，请核对连接配置、网络与建表权限 (%T)", err)
		}
		defer database.Close()
		store = database
		workStore = database
	}

	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatalf("监听 HTTP 地址失败: %v", err)
	}
	server := &http.Server{
		Handler: httpapi.NewHandler(store, adminToken, web, httpapi.WorkspaceOptions{
			Store: workStore, PluginToken: pluginToken,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("muster-hub 正在监听 http://%s", listener.Addr())
	log.Fatal(server.Serve(listener))
}
