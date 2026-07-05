// Command stallwatch monitors that work is actually happening — not just
// that services are up.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Albaraazain/stallwatch/internal/alert"
	"github.com/Albaraazain/stallwatch/internal/config"
	"github.com/Albaraazain/stallwatch/internal/engine"
	"github.com/Albaraazain/stallwatch/internal/store"
)

var version = "dev" // overridden at build time via -ldflags

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "stallwatch:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "stallwatch.yaml", "path to config file")
	dbPath := flag.String("db", "stallwatch.db", "path to the sample database")
	checkOnly := flag.Bool("check", false, "validate the config and exit")
	debug := flag.Bool("debug", false, "enable debug logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("stallwatch", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *checkOnly {
		fmt.Printf("config ok: %d signals, %d alert sinks\n", len(cfg.Signals), len(cfg.Alerts))
		return nil
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	sinks := make([]alert.Sink, 0, len(cfg.Alerts))
	for _, a := range cfg.Alerts {
		sinks = append(sinks, alert.NewWebhook(a))
	}

	eng, err := engine.New(cfg, st, sinks, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("stallwatch started",
		"version", version, "signals", len(cfg.Signals), "sinks", len(cfg.Alerts),
		"retention", cfg.Defaults.Retention.Std())
	eng.Run(ctx)
	log.Info("stallwatch stopped")
	return nil
}
