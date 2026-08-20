// Package config loads and validates reflector configuration and secrets.
package config

import (
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"strings"
	"time"
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
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Network.UDPListen == "" || cfg.Reflector.ID == "" || cfg.Reflector.DisplayName == "" {
		return Config{}, errors.New("network and reflector identity are required")
	}
	if cfg.Limits.MaxDatagramBytes < 32 || cfg.Limits.MaxDatagramBytes > 1200 {
		return Config{}, errors.New("max datagram bytes must be 32 through 1200")
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
	data, err := os.ReadFile(c.Authentication.SharedKeyFile)
	if err != nil {
		return nil, err
	}
	if len(data) > 65 {
		return nil, errors.New("shared key file is too large")
	}
	data = []byte(strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"))
	return validateKey(data)
}
func validateKey(key []byte) ([]byte, error) {
	if len(key) < 16 || len(key) > 64 {
		return nil, fmt.Errorf("shared key length must be 16 through 64 bytes")
	}
	return append([]byte(nil), key...), nil
}
