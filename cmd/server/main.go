// today2api-panel：Today（todayai.cn / today.ai）账号 → OpenAI / Codex / Claude 兼容网关。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/misakano7545/today2api-panel/internal/config"
	"github.com/misakano7545/today2api-panel/internal/server"
)

var buildVersion = "dev"

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	version := flag.String("version", buildVersion, "版本号（发布时注入）")
	flag.Parse()

	store, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cfg := store.Snapshot()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(store, *version).Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// 对话是长请求：不设 WriteTimeout，由 ReplyTimeoutSec 兜底。
	}

	go func() {
		log.Printf("today2api-panel %s listening on %s (accounts=%d keys=%d)", *version, cfg.Listen, len(cfg.Accounts), len(cfg.Keys))
		if cfg.PanelPassword == "" {
			log.Printf("panel: password not set — 仅本机可管理面板，对外暴露前请设置 panel_password")
		}
		if errListen := srv.ListenAndServe(); errListen != nil && !errors.Is(errListen, http.ErrServerClosed) {
			log.Fatalf("listen: %v", errListen)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Printf("bye")
}
