// Command opusrefd runs the OpusRef reflector.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/dbehnke/opusref/internal/config"
	"github.com/dbehnke/opusref/pkg/monitor"
	"github.com/dbehnke/opusref/pkg/server"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "config.yaml", "YAML configuration file")
	flag.Parse()
	cfg, err := config.Load(*path)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	key, err := cfg.SharedKey()
	if err != nil {
		return fmt.Errorf("load shared key: %w", err)
	}
	conn, err := net.ListenPacket("udp", cfg.Network.UDPListen)
	if err != nil {
		return fmt.Errorf("bind UDP: %w", err)
	}
	reflector, err := server.NewReflector(conn, server.ReflectorOptions{ID: cfg.Reflector.ID, DisplayName: cfg.Reflector.DisplayName, SharedKey: key, Limits: server.Limits{MaxClients: cfg.Limits.MaxClients, MaxCompletedTransactions: cfg.Limits.MaxCompletedTransactions, SessionTimeout: cfg.Timers.SessionTimeout, GrantTimeout: cfg.Timers.GrantTimeout, MediaTimeout: cfg.Timers.StreamInactivityTimeout, TransmitTimeLimit: cfg.Timers.TransmitTimeLimit}})
	if err != nil {
		conn.Close()
		return err
	}
	registry := monitor.New(cfg.Limits.RecentEvents, cfg.Timers.HealthDeadline, func(time.Duration) bool { return true })
	registry.Publish(monitor.Snapshot{Ready: true, ReflectorID: cfg.Reflector.ID})
	httpServer := &http.Server{Addr: cfg.Monitoring.HTTPListen, Handler: registry.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errHTTP := make(chan error, 1)
	go func() { errHTTP <- httpServer.ListenAndServe() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errRun := make(chan error, 1)
	go func() { errRun <- reflector.Run(ctx) }()
	select {
	case <-ctx.Done():
	case err = <-errHTTP:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
		}
	case err = <-errRun:
		stop()
	}
	registry.Publish(monitor.Snapshot{Ready: false, ReflectorID: cfg.Reflector.ID})
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Timers.ShutdownGrace)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	_ = reflector.Close()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
