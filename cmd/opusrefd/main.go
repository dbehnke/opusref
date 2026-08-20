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
	registry.Publish(monitor.Snapshot{Ready: false, ReflectorID: cfg.Reflector.ID})
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
					_ = registry.Add("opusref_queue_drops_total", map[string]string{"queue": "server_inbound", "item_type": "datagram"}, s.InboundDrops-priorInbound)
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
				registry.Publish(monitor.Snapshot{Ready: s.Ready, ReflectorID: cfg.Reflector.ID, Clients: len(clients), ClientList: clients, Floor: s.Floor, Stream: monitor.StreamSnapshot{Active: s.Floor.Active, SessionID: s.Floor.SessionID, StreamID: s.Floor.StreamID, NodeCallsign: s.Floor.NodeCallsign, SourceCallsign: s.Floor.SourceCallsign}})
			}
		}
	}()
	select {
	case <-ctx.Done():
	case err = <-errHTTP:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
		}
	case err = <-errRun:
		stop()
	}
	close(monitorDone)
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
