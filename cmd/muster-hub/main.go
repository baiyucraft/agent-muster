package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/httpapi"
)

func main() {
	address := flag.String("listen", "127.0.0.1:8080", "HTTP 监听地址")
	version := flag.Bool("version", false, "显示版本")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("muster-hub 仅接受命令行选项")
	}
	if *version {
		fmt.Println("muster-hub dev")
		return
	}

	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatalf("监听 HTTP 地址失败: %v", err)
	}
	server := &http.Server{
		Handler:           httpapi.NewHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("muster-hub 正在监听 http://%s", listener.Addr())
	log.Fatal(server.Serve(listener))
}
