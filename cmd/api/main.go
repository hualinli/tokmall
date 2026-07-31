package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/lihualin/tokmall/internal/config"
	"github.com/lihualin/tokmall/internal/health"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	addr := fmt.Sprintf(":%s", cfg.ServerPort)
	h := server.New(server.WithHostPorts(addr))

	health.Register(h)
	return h.Run()
}
