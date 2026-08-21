// Package config loads and validates reflector configuration and secrets.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/dbehnke/opusref/pkg/wire"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"os"
	"time"
	"unicode/utf8"
)

type Config struct {
	Network        Network        `yaml:"network"`
	Reflector      Reflector      `yaml:"reflector"`
	Limits         Limits         `yaml:"limits"`
	Timers         Timers         `yaml:"timers"`
	Authentication Authentication `yaml:"authentication"`
	Monitoring     Monitoring     `yaml:"monitoring"`
	Logging        Logging        `yaml:"logging"`
}
type Network struct {
	UDPListen string `yaml:"udp_listen"`
}
type Reflector struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
}
type Limits struct {
	MaxClients                         int `yaml:"max_clients"`
	MaxDatagramBytes                   int `yaml:"max_datagram_bytes"`
	InboundQueuePackets                int `yaml:"inbound_queue_packets"`
	OutboundMediaQueueFrames           int `yaml:"outbound_media_queue_frames"`
	OutboundControlQueuePackets        int `yaml:"outbound_control_queue_packets"`
	MaxPendingChallenges               int `yaml:"max_pending_challenges"`
	MaxCompletedTransactions           int `yaml:"max_completed_transactions"`
	MaxCompletedTransactionsPerSession int `yaml:"max_completed_transactions_per_session"`
	MaxPendingNotifications            int `yaml:"max_pending_notifications"`
	MaxPendingNotificationsPerClient   int `yaml:"max_pending_notifications_per_client"`
	RecentEvents                       int `yaml:"recent_events"`
}
type Timers struct {
	KeepaliveInterval       time.Duration `yaml:"keepalive_interval"`
	SessionTimeout          time.Duration `yaml:"session_timeout"`
	GrantTimeout            time.Duration `yaml:"grant_timeout"`
	StreamInactivityTimeout time.Duration `yaml:"stream_inactivity_timeout"`
	TransmitTimeLimit       time.Duration `yaml:"transmit_time_limit"`
	HealthDeadline          time.Duration `yaml:"health_deadline"`
	ShutdownGrace           time.Duration `yaml:"shutdown_grace"`
}
type Authentication struct {
	SharedKeyEnv  string `yaml:"shared_key_env"`
	SharedKeyFile string `yaml:"shared_key_file"`
}
type Monitoring struct {
	HTTPListen string `yaml:"http_listen"`
}
type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

func Defaults() Config {
	return Config{Network: Network{":46000"}, Limits: Limits{100, 1200, 256, 256, 64, 100, 1024, 64, 200, 2, 256}, Timers: Timers{10 * time.Second, 30 * time.Second, 2 * time.Second, time.Second, 180 * time.Second, 250 * time.Millisecond, 5 * time.Second}, Authentication: Authentication{SharedKeyEnv: "OPUSREF_SHARED_KEY"}, Monitoring: Monitoring{"127.0.0.1:8080"}, Logging: Logging{"info", "json"}}
}
func Load(path string) (Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err = decoder.Decode(&cfg); err != nil {
		return Config{}, err
	}
	if cfg.Network.UDPListen == "" || cfg.Reflector.ID == "" || cfg.Reflector.DisplayName == "" {
		return Config{}, errors.New("network and reflector identity are required")
	}
	if _, err := wire.ReflectorID(cfg.Reflector.ID); err != nil {
		return Config{}, err
	}
	if len(cfg.Reflector.DisplayName) > 64 || !utf8.ValidString(cfg.Reflector.DisplayName) {
		return Config{}, errors.New("reflector display name is invalid")
	}
	if cfg.Limits.MaxDatagramBytes < 32 || cfg.Limits.MaxDatagramBytes > 1200 {
		return Config{}, errors.New("max datagram bytes must be 32 through 1200")
	}
	if _, err := net.ResolveUDPAddr("udp", cfg.Network.UDPListen); err != nil {
		return Config{}, fmt.Errorf("invalid UDP address: %w", err)
	}
	if cfg.Monitoring.HTTPListen != "" {
		if _, err := net.ResolveTCPAddr("tcp", cfg.Monitoring.HTTPListen); err != nil {
			return Config{}, fmt.Errorf("invalid monitoring address: %w", err)
		}
	}
	capacities := []int{cfg.Limits.MaxClients, cfg.Limits.InboundQueuePackets, cfg.Limits.OutboundMediaQueueFrames, cfg.Limits.OutboundControlQueuePackets, cfg.Limits.MaxPendingChallenges, cfg.Limits.MaxCompletedTransactions, cfg.Limits.MaxCompletedTransactionsPerSession, cfg.Limits.MaxPendingNotifications, cfg.Limits.MaxPendingNotificationsPerClient, cfg.Limits.RecentEvents}
	for _, value := range capacities {
		if value <= 0 {
			return Config{}, errors.New("all capacity limits must be positive")
		}
	}
	if cfg.Limits.MaxCompletedTransactionsPerSession > cfg.Limits.MaxCompletedTransactions || cfg.Limits.MaxPendingNotificationsPerClient > cfg.Limits.MaxPendingNotifications {
		return Config{}, errors.New("a per-client limit exceeds its global limit")
	}
	timers := []time.Duration{cfg.Timers.KeepaliveInterval, cfg.Timers.SessionTimeout, cfg.Timers.GrantTimeout, cfg.Timers.StreamInactivityTimeout, cfg.Timers.TransmitTimeLimit, cfg.Timers.HealthDeadline, cfg.Timers.ShutdownGrace}
	for _, value := range timers {
		if value <= 0 {
			return Config{}, errors.New("all timers must be positive")
		}
	}
	if cfg.Timers.KeepaliveInterval >= cfg.Timers.SessionTimeout {
		return Config{}, errors.New("keepalive interval must be less than session timeout")
	}
	if cfg.Logging.Level != "debug" && cfg.Logging.Level != "info" && cfg.Logging.Level != "warn" && cfg.Logging.Level != "error" {
		return Config{}, errors.New("logging level is invalid")
	}
	if cfg.Logging.Format != "json" && cfg.Logging.Format != "text" {
		return Config{}, errors.New("logging format is invalid")
	}
	return cfg, nil
}
func (c Config) SharedKey() ([]byte, error) {
	if c.Authentication.SharedKeyEnv != "" {
		if value := os.Getenv(c.Authentication.SharedKeyEnv); value != "" {
			return validateKey([]byte(value))
		}
	}
	if c.Authentication.SharedKeyFile == "" {
		return nil, nil
	}
	info, err := os.Stat(c.Authentication.SharedKeyFile)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("shared key file must be regular and accessible only by its owner")
	}
	data, err := ReadSharedKeyFile(c.Authentication.SharedKeyFile)
	if err != nil {
		return nil, err
	}
	return validateKey(data)
}
func ReadSharedKeyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 66))
	if err != nil {
		return nil, err
	}
	if len(data) > 65 {
		return nil, errors.New("shared key file is too large")
	}
	if bytes.HasSuffix(data, []byte("\r\n")) {
		data = data[:len(data)-2]
	} else if bytes.HasSuffix(data, []byte("\n")) {
		data = data[:len(data)-1]
	}
	return data, nil
}
func validateKey(key []byte) ([]byte, error) {
	if len(key) < 16 || len(key) > 64 {
		return nil, fmt.Errorf("shared key length must be 16 through 64 bytes")
	}
	return append([]byte(nil), key...), nil
}
