// Command opusrefweb runs the web companion and local account tools.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/dbehnke/opusref/internal/webapp/auth"
	webconfig "github.com/dbehnke/opusref/internal/webapp/config"
	"github.com/dbehnke/opusref/internal/webapp/gateway"
	"github.com/dbehnke/opusref/internal/webapp/httpapi"
	wsprotocol "github.com/dbehnke/opusref/internal/webapp/socket"
	"github.com/dbehnke/opusref/internal/webapp/store"
	"github.com/dbehnke/opusref/pkg/client"
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
	policy := auth.Policy{Username: *username, Callsign: *callsign, ServiceTerms: []string{c.WebAuthn.RPName}}
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
func serve(args []string) error {
	c, err := load(args, "serve")
	if err != nil {
		return err
	}
	state, err := store.Open(context.Background(), c.Storage.SQLitePath)
	if err != nil {
		return err
	}
	defer state.Close()
	sharedKey := ""
	if c.Reflector.SharedKeyEnv != "" {
		sharedKey = os.Getenv(c.Reflector.SharedKeyEnv)
	}
	clientOptions := client.Options{ServerAddress: c.Reflector.UDPAddress, NodeCallsign: c.Reflector.NodeCallsign, SharedKey: sharedKey}
	receiver, err := client.NewUDP(clientOptions)
	if err != nil {
		return err
	}
	defer receiver.Close()
	transmitter, err := client.NewUDP(clientOptions)
	if err != nil {
		return err
	}
	defer transmitter.Close()
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	if err = receiver.Connect(runCtx); err != nil {
		return fmt.Errorf("connect receiver: %w", err)
	}
	if err = transmitter.Connect(runCtx); err != nil {
		return fmt.Errorf("connect transmitter: %w", err)
	}
	hub := gateway.NewLiveHub()
	ptt := gateway.NewPTTManager(transmitter)
	go publishReceiver(runCtx, receiver, hub)
	api := httpapi.New(httpapi.Config{PublicOrigin: c.Web.PublicOrigin, OpenAccess: c.Web.OpenAccess, SessionIdle: c.Authentication.SessionIdle.Time(), SessionAbsolute: c.Authentication.SessionAbsolute.Time(), MaxSessions: c.Authentication.MaxSessionsPerAccount, Argon2: params(c), Assets: webassets.Handler(), LiveHub: hub, PTT: ptt, LiveQueuePackets: c.Limits.LiveQueuePackets, MaxConcurrentHashes: c.Authentication.MaxConcurrentHashes}, state)
	public := &http.Server{Addr: c.Web.HTTPListen, Handler: api.PublicHandler(), ReadHeaderTimeout: 5 * time.Second}
	monitor := &http.Server{Addr: c.Web.MonitorListen, Handler: api.MonitorHandler(), ReadHeaderTimeout: 5 * time.Second}
	fail := make(chan error, 2)
	go func() { fail <- public.ListenAndServe() }()
	go func() { fail <- monitor.ListenAndServe() }()
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
	runCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(public.Shutdown(ctx), monitor.Shutdown(ctx))
}

func publishReceiver(ctx context.Context, receiver client.Client, hub *gateway.LiveHub) {
	var channel uint64
	var sequence uint32
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-receiver.Events():
			switch event.Kind {
			case client.EventStreamStart:
				channel = randomChannel()
				sequence = 0
			case client.EventAudio:
				if channel != 0 {
					hub.Publish(wsprotocol.Media{Kind: wsprotocol.KindLive, ChannelID: channel, Sequence: sequence, Timestamp: event.Timestamp, Payload: event.Payload})
					sequence++
				}
			case client.EventStreamEnd:
				channel = 0
			}
		}
	}
}
func randomChannel() uint64 {
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return 1
		}
		if id := binary.BigEndian.Uint64(raw[:]); id != 0 {
			return id
		}
	}
}
