// Command opusrefweb runs the web companion and local account tools.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	reflectorconfig "github.com/dbehnke/opusref/internal/config"
	webarchive "github.com/dbehnke/opusref/internal/webapp/archive"
	"github.com/dbehnke/opusref/internal/webapp/auth"
	webconfig "github.com/dbehnke/opusref/internal/webapp/config"
	"github.com/dbehnke/opusref/internal/webapp/gateway"
	"github.com/dbehnke/opusref/internal/webapp/httpapi"
	"github.com/dbehnke/opusref/internal/webapp/passkey"
	reflectormonitor "github.com/dbehnke/opusref/internal/webapp/reflector"
	wsprotocol "github.com/dbehnke/opusref/internal/webapp/socket"
	"github.com/dbehnke/opusref/internal/webapp/store"
	"github.com/dbehnke/opusref/pkg/client"
	"github.com/dbehnke/opusref/pkg/wire"
	webassets "github.com/dbehnke/opusref/web"
	"golang.org/x/term"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: opusrefweb serve|auth benchmark|admin create")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "auth":
		if len(args) > 1 && args[1] == "benchmark" {
			return benchmark(args[2:])
		}
	case "admin":
		if len(args) > 1 && args[1] == "create" {
			return createAdmin(args[2:])
		}
		if len(args) > 1 && args[1] == "recover" {
			return recoverAdmin(args[2:])
		}
	}
	return errors.New("unknown command")
}
func load(args []string, name string) (webconfig.Config, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", "", "configuration file")
	if err := fs.Parse(args); err != nil {
		return webconfig.Config{}, err
	}
	if *path == "" {
		return webconfig.Config{}, errors.New("--config is required")
	}
	return webconfig.Load(*path)
}
func params(c webconfig.Config) auth.Params {
	return auth.Params{MemoryKiB: c.Authentication.Argon2MemoryKiB, Iterations: c.Authentication.Argon2Iterations, Parallelism: c.Authentication.Argon2Parallelism, SaltBytes: 16, KeyBytes: 32}
}
func benchmark(args []string) error {
	c, err := load(args, "auth benchmark")
	if err != nil {
		return err
	}
	p := params(c)
	_, _ = auth.HashPassword("benchmark-only-password", p)
	samples := make([]time.Duration, 5)
	for i := range samples {
		start := time.Now()
		if _, err = auth.HashPassword("benchmark-only-password", p); err != nil {
			return err
		}
		samples[i] = time.Since(start)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	fmt.Printf("median=%s maximum=%s memory_kib=%d\n", samples[2], samples[4], p.MemoryKiB)
	if samples[2] > 500*time.Millisecond || samples[4] > time.Second {
		return errors.New("Argon2 benchmark exceeds the permitted duration")
	}
	return nil
}
func createAdmin(args []string) error {
	fs := flag.NewFlagSet("admin create", flag.ContinueOnError)
	path := fs.String("config", "", "configuration file")
	username := fs.String("username", "", "normalized username")
	callsign := fs.String("callsign", "", "optional source callsign")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *username == "" {
		return errors.New("--config and --username are required")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("password input requires a TTY")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	c, err := webconfig.Load(*path)
	if err != nil {
		return err
	}
	additional, err := auth.LoadAdditionalBlocklist(c.Authentication.PasswordBlocklistFile)
	if err != nil {
		return err
	}
	policy := auth.Policy{Username: *username, Callsign: *callsign, ServiceTerms: []string{c.WebAuthn.RPName}, Additional: additional}
	if policyErr := policy.Check(string(raw)); policyErr != nil {
		return policyErr
	}
	hash, err := auth.HashPassword(string(raw), params(c))
	for i := range raw {
		raw[i] = 0
	}
	if err != nil {
		return err
	}
	state, err := store.Open(context.Background(), c.Storage.SQLitePath)
	if err != nil {
		return err
	}
	defer state.Close()
	_, err = state.CreateUser(context.Background(), store.CreateUser{Username: *username, Role: store.RoleAdmin, Callsign: *callsign, PasswordHash: hash, PasswordChangeRequired: true})
	return err
}
func recoverAdmin(args []string) error {
	fs := flag.NewFlagSet("admin recover", flag.ContinueOnError)
	path := fs.String("config", "", "configuration file")
	username := fs.String("username", "", "account username")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *username == "" {
		return errors.New("--config and --username are required")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("password input requires a TTY")
	}
	fmt.Fprint(os.Stderr, "New temporary password: ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	c, err := webconfig.Load(*path)
	if err != nil {
		return err
	}
	additional, err := auth.LoadAdditionalBlocklist(c.Authentication.PasswordBlocklistFile)
	if err != nil {
		return err
	}
	policy := auth.Policy{Username: *username, ServiceTerms: []string{c.WebAuthn.RPName}, Additional: additional}
	if policyErr := policy.Check(string(raw)); policyErr != nil {
		return policyErr
	}
	hash, err := auth.HashPassword(string(raw), params(c))
	for index := range raw {
		raw[index] = 0
	}
	if err != nil {
		return err
	}
	state, err := store.Open(context.Background(), c.Storage.SQLitePath)
	if err != nil {
		return err
	}
	defer state.Close()
	return state.RecoverAdmin(context.Background(), *username, hash, time.Now())
}
func serve(args []string) error {
	c, err := load(args, "serve")
	if err != nil {
		return err
	}
	argonStarted := time.Now()
	if _, err = auth.HashPassword("startup-argon2-latency-probe", params(c)); err != nil {
		return fmt.Errorf("Argon2 startup probe: %w", err)
	}
	if elapsed := time.Since(argonStarted); elapsed > 500*time.Millisecond {
		_, _ = fmt.Fprintf(os.Stderr, "warning: Argon2 startup probe took %s; run opusrefweb auth benchmark\n", elapsed.Round(time.Millisecond))
	}
	state, err := store.Open(context.Background(), c.Storage.SQLitePath)
	if err != nil {
		return err
	}
	defer state.Close()
	sharedKeyBytes, err := reflectorconfig.ResolveSharedKey(c.Reflector.SharedKeyEnv, c.Reflector.SharedKeyFile)
	if err != nil {
		return err
	}
	sharedKey := string(sharedKeyBytes)
	clientOptions := client.Options{ServerAddress: c.Reflector.UDPAddress, NodeCallsign: c.Reflector.NodeCallsign, SharedKey: sharedKey}
	receiver := gateway.NewSupervisedClientWithBackoff(func() (client.Client, error) { return client.NewUDP(clientOptions) }, 512, c.Reflector.ReconnectInitial.Time(), c.Reflector.ReconnectMax.Time())
	defer receiver.Close()
	transmitter := gateway.NewSupervisedClientWithBackoff(func() (client.Client, error) { return client.NewUDP(clientOptions) }, 512, c.Reflector.ReconnectInitial.Time(), c.Reflector.ReconnectMax.Time())
	defer transmitter.Close()
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	_ = receiver.Connect(runCtx)
	_ = transmitter.Connect(runCtx)
	hub := gateway.NewLiveHub()
	ptt := gateway.NewPTTManager(transmitter)
	attribution := gateway.NewGrantAttribution()
	ptt.SetAttribution(attribution)
	go observeTransmitter(runCtx, transmitter, attribution, ptt)
	archives, err := webarchive.NewService(runCtx, state, c.Storage.ArchiveDirectory, c.Storage.HardQuotaBytes, c.Limits.ArchiveQueuePackets)
	if err != nil {
		return err
	}
	defer archives.Close()
	go func() {
		ticker := time.NewTicker(c.Storage.PurgeInterval.Time())
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case now := <-ticker.C:
				_ = archives.Purge(runCtx, c.Storage.Retention.Time(), now)
				_, _ = state.PurgeAudit(runCtx, now.Add(-c.Storage.AuditRetention.Time()))
				_, _ = state.PurgeTombstones(runCtx, now.Add(-30*24*time.Hour))
			}
		}
	}()
	passkeys, err := passkey.New(c.WebAuthn.RPID, c.WebAuthn.RPName, c.WebAuthn.Origins, state)
	if err != nil {
		return err
	}
	additional, err := auth.LoadAdditionalBlocklist(c.Authentication.PasswordBlocklistFile)
	if err != nil {
		return err
	}
	monitorClient := reflectormonitor.New(c.Reflector.MonitoringURL)
	go monitorClient.Run(runCtx, c.Reflector.MonitorPollInterval.Time())
	go publishReceiver(runCtx, receiver, hub, archives, attribution)
	api := httpapi.New(httpapi.Config{PublicOrigin: c.Web.PublicOrigin, OpenAccess: c.Web.OpenAccess, SessionIdle: c.Authentication.SessionIdle.Time(), SessionAbsolute: c.Authentication.SessionAbsolute.Time(), MaxSessions: c.Authentication.MaxSessionsPerAccount, Argon2: params(c), Assets: webassets.Handler(), LiveHub: hub, PTT: ptt, LiveQueuePackets: c.Limits.LiveQueuePackets, LiveQueueBytes: c.Limits.LiveQueueBytes, PlaybackQueuePackets: c.Limits.PlaybackQueuePackets, PlaybackQueueBytes: c.Limits.PlaybackQueueBytes, ControlQueueMessages: c.Limits.ControlQueueMessages, MaxPlaybacks: c.Limits.MaxPlaybacks, PlaybackMaxDuration: c.Limits.PlaybackMaxDuration.Time(), MaxConcurrentHashes: c.Authentication.MaxConcurrentHashes, Passkeys: passkeys, TrustedProxyCIDRs: c.Web.TrustedProxyCIDRs, MaxWebSockets: c.Limits.MaxWebSockets, MaxWebSocketsPerSession: c.Limits.MaxWebSocketsPerSession, PasswordBlocklist: additional, ReflectorMonitor: monitorClient, MonitorStaleAfter: c.Reflector.MonitorStaleAfter.Time(), Archives: archives, ReadyCheck: func() bool { return receiver.Ready() && transmitter.Ready() && archives.Probe(context.Background()) }}, state)
	api.SetReady(false)
	public := &http.Server{Addr: c.Web.HTTPListen, Handler: api.PublicHandler(), ReadHeaderTimeout: 5 * time.Second}
	monitor := &http.Server{Addr: c.Web.MonitorListen, Handler: api.MonitorHandler(), ReadHeaderTimeout: 5 * time.Second}
	publicListener, err := net.Listen("tcp", c.Web.HTTPListen)
	if err != nil {
		return fmt.Errorf("bind public listener: %w", err)
	}
	monitorListener, err := net.Listen("tcp", c.Web.MonitorListen)
	if err != nil {
		_ = publicListener.Close()
		return fmt.Errorf("bind monitoring listener: %w", err)
	}
	fail := make(chan error, 2)
	go func() { fail <- public.Serve(publicListener) }()
	go func() { fail <- monitor.Serve(monitorListener) }()
	api.SetReady(true)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-signals:
	case err = <-fail:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	api.SetReady(false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpErr := errors.Join(public.Shutdown(ctx), monitor.Shutdown(ctx))
	api.Shutdown()
	_ = ptt.StopActive(ctx)
	runCancel()
	return httpErr
}

func publishReceiver(ctx context.Context, receiver client.Client, hub *gateway.LiveHub, archives *webarchive.Service, attribution *gateway.GrantAttribution) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-receiver.Events():
			key := webarchive.StreamKey{SessionID: event.SessionID, StreamID: event.StreamID}
			switch event.Kind {
			case client.EventStreamStart:
				hub.Start(event.SourceCallsign)
				webUserID := attribution.User(gateway.ReflectorStream{SessionID: event.SessionID, StreamID: event.StreamID})
				archives.Start(key, event.NodeCallsign, event.SourceCallsign, webUserID, time.Now())
			case client.EventAudio:
				hub.Publish(wsprotocol.Media{Kind: wsprotocol.KindLive, Timestamp: event.Timestamp, Payload: event.Payload})
				archives.Audio(key, event.Sequence, event.Timestamp, event.Payload, time.Now())
			case client.EventData:
				archives.Data(key, event.Sequence)
			case client.EventStreamEnd:
				hub.End("stream_end")
				archives.End(key, recordingEndReason(event.EndReason), event.Synthetic)
			}
		}
	}
}

func observeTransmitter(ctx context.Context, transmitter client.Client, attribution *gateway.GrantAttribution, ptt *gateway.PTTManager) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-transmitter.Events():
			if !ok {
				return
			}
			attribution.Observe(event)
			ptt.Observe(event)
		}
	}
}

func recordingEndReason(reason wire.EndReason) string {
	switch reason {
	case wire.EndReasonOwnerDisconnect:
		return "owner_disconnect"
	case wire.EndReasonGrantTimeout:
		return "grant_timeout"
	case wire.EndReasonMediaInactivity:
		return "media_inactivity"
	case wire.EndReasonTransmitTimeLimit:
		return "transmit_time_limit"
	case wire.EndReasonServerShutdown:
		return "server_shutdown"
	default:
		return "normal"
	}
}
