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
	"io"
	"log/slog"
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
	logger := newLogger(cfg.Logging, os.Stderr)
	key, err := cfg.SharedKey()
	if err != nil {
		return fmt.Errorf("load shared key: %w", err)
	}
	conn, err := net.ListenPacket("udp", cfg.Network.UDPListen)
	if err != nil {
		return fmt.Errorf("bind UDP: %w", err)
	}
	reflector, err := server.NewReflector(conn, server.ReflectorOptions{ID: cfg.Reflector.ID, DisplayName: cfg.Reflector.DisplayName, SharedKey: key, Limits: server.Limits{MaxClients: cfg.Limits.MaxClients, MaxCompletedTransactions: cfg.Limits.MaxCompletedTransactions, SessionTimeout: cfg.Timers.SessionTimeout, GrantTimeout: cfg.Timers.GrantTimeout, MediaTimeout: cfg.Timers.StreamInactivityTimeout, TransmitTimeLimit: cfg.Timers.TransmitTimeLimit}, MaxDatagramBytes: cfg.Limits.MaxDatagramBytes, InboundQueuePackets: cfg.Limits.InboundQueuePackets, OutboundControlQueuePackets: cfg.Limits.OutboundControlQueuePackets, OutboundMediaQueueFrames: cfg.Limits.OutboundMediaQueueFrames, MaxPendingChallenges: cfg.Limits.MaxPendingChallenges, MaxPendingNotifications: cfg.Limits.MaxPendingNotifications, MaxPendingNotificationsPerClient: cfg.Limits.MaxPendingNotificationsPerClient, MaxCompletedTransactionsPerSession: cfg.Limits.MaxCompletedTransactionsPerSession, ShutdownGrace: cfg.Timers.ShutdownGrace})
	if err != nil {
		conn.Close()
		return err
	}
	registry := monitor.New(cfg.Limits.RecentEvents, cfg.Timers.HealthDeadline, func(deadline time.Duration) bool { return time.Since(reflector.Snapshot().Updated) <= deadline })
	reflector.SetMetricSink(registry)
	reflector.SetEventSink(func(event server.EventRecord) {
		registry.AddEvent(monitor.Event{Type: event.Type, Severity: event.Severity, Details: event.Details})
	})
	logRuntimeStart(logger, cfg)
	registry.Publish(monitor.Snapshot{Ready: false, ReflectorID: cfg.Reflector.ID, DisplayName: cfg.Reflector.DisplayName})
	httpServer := &http.Server{Addr: cfg.Monitoring.HTTPListen, Handler: registry.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errHTTP := make(chan error, 1)
	if cfg.Monitoring.HTTPListen != "" {
		go func() { errHTTP <- httpServer.ListenAndServe() }()
	} else {
		errHTTP = nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errRun := make(chan error, 1)
	go func() { errRun <- reflector.Run(ctx) }()
	monitorDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		var priorInbound, priorGaps uint64
		for {
			select {
			case <-monitorDone:
				return
			case <-ticker.C:
				s := reflector.Snapshot()
				if s.InboundDrops > priorInbound {
					dropped := s.InboundDrops - priorInbound
					_ = registry.Add("opusref_queue_drops_total", map[string]string{"queue": "server_inbound", "item_type": "datagram"}, dropped)
					registry.AddEvent(monitor.Event{Type: "queue_drop", Severity: "warn", Details: map[string]any{"queue": "server_inbound", "item_type": "datagram", "frame_count": dropped, "recipient_count": uint64(0)}})
					priorInbound = s.InboundDrops
				}
				if s.SequenceGaps > priorGaps {
					_ = registry.Add("opusref_sequence_gaps_total", map[string]string{"direction": "rx"}, s.SequenceGaps-priorGaps)
					priorGaps = s.SequenceGaps
				}
				clients := make([]monitor.ClientSnapshot, 0, len(s.Clients))
				for _, c := range s.Clients {
					clients = append(clients, monitor.ClientSnapshot{NodeCallsign: c.NodeCallsign, RemoteAddress: c.RemoteAddress, SessionID: c.SessionID, ConnectedAt: c.ConnectedAt, LastActivity: c.LastActivity})
				}
				registry.Publish(monitor.Snapshot{Ready: s.Ready, ReflectorID: cfg.Reflector.ID, DisplayName: cfg.Reflector.DisplayName, Clients: len(clients), ClientList: clients, Floor: s.Floor, Stream: monitor.StreamSnapshot{Active: s.Floor.Active, SessionID: s.Floor.SessionID, StreamID: s.Floor.StreamID, NodeCallsign: s.Floor.NodeCallsign, SourceCallsign: s.Floor.SourceCallsign, StartedAt: s.Floor.StartedAt, LastFrameAt: s.Floor.LastFrameAt, RemainingTransmitSeconds: s.Floor.RemainingTransmitTime.Seconds()}})
			}
		}
	}()
	err = waitForTermination(ctx, stop, errHTTP, errRun)
	close(monitorDone)
	registry.Publish(monitor.Snapshot{Ready: false, ReflectorID: cfg.Reflector.ID, DisplayName: cfg.Reflector.DisplayName})
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Timers.ShutdownGrace)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	_ = reflector.Close()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("reflector stopped with an error", "error", err)
		return err
	}
	logger.Info("reflector stopped")
	return nil
}

func newLogger(logging config.Logging, output io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch logging.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	options := &slog.HandlerOptions{Level: level}
	if logging.Format == "text" {
		return slog.New(slog.NewTextHandler(output, options))
	}
	return slog.New(slog.NewJSONHandler(output, options))
}

func logRuntimeStart(logger *slog.Logger, cfg config.Config) {
	logger.Info("reflector started", "reflector_id", cfg.Reflector.ID, "udp_listen", cfg.Network.UDPListen, "monitoring_listen", cfg.Monitoring.HTTPListen)
}
func waitForTermination(ctx context.Context, stop context.CancelFunc, httpErrors, runErrors <-chan error) error {
	select {
	case <-ctx.Done():
		return <-runErrors
	case err := <-httpErrors:
		stop()
		runErr := <-runErrors
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return runErr
	case err := <-runErrors:
		stop()
		return err
	}
}
