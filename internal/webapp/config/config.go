// Package config loads the strict opusrefweb configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbehnke/opusref/pkg/wire"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Web            Web            `yaml:"web"`
	Reflector      Reflector      `yaml:"reflector"`
	Storage        Storage        `yaml:"storage"`
	Authentication Authentication `yaml:"authentication"`
	WebAuthn       WebAuthn       `yaml:"webauthn"`
	Limits         Limits         `yaml:"limits"`
	Logging        Logging        `yaml:"logging"`
}

// Duration accepts a duration string in YAML.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return errors.New("duration is invalid")
	}
	*d = Duration(parsed)
	return nil
}
func (d Duration) Time() time.Duration { return time.Duration(d) }

type Web struct {
	HTTPListen        string   `yaml:"http_listen"`
	MonitorListen     string   `yaml:"monitor_listen"`
	PublicOrigin      string   `yaml:"public_origin"`
	OpenAccess        bool     `yaml:"open_access"`
	TrustedProxyCIDRs []string `yaml:"trusted_proxy_cidrs"`
}
type Reflector struct {
	UDPAddress          string   `yaml:"udp_address"`
	MonitoringURL       string   `yaml:"monitoring_url"`
	NodeCallsign        string   `yaml:"node_callsign"`
	MonitorPollInterval Duration `yaml:"monitor_poll_interval"`
	MonitorStaleAfter   Duration `yaml:"monitor_stale_after"`
	ReconnectInitial    Duration `yaml:"reconnect_initial"`
	ReconnectMax        Duration `yaml:"reconnect_max"`
	SharedKeyEnv        string   `yaml:"shared_key_env"`
	SharedKeyFile       string   `yaml:"shared_key_file"`
}
type Storage struct {
	SQLitePath       string   `yaml:"sqlite_path"`
	ArchiveDirectory string   `yaml:"archive_directory"`
	Retention        Duration `yaml:"retention"`
	HardQuotaBytes   int64    `yaml:"hard_quota_bytes"`
	PurgeInterval    Duration `yaml:"purge_interval"`
	AuditRetention   Duration `yaml:"audit_retention"`
}
type Authentication struct {
	SessionIdle           Duration `yaml:"session_idle"`
	SessionAbsolute       Duration `yaml:"session_absolute"`
	MaxSessionsPerAccount int      `yaml:"max_sessions_per_account"`
	Argon2MemoryKiB       uint32   `yaml:"argon2_memory_kib"`
	Argon2Iterations      uint32   `yaml:"argon2_iterations"`
	Argon2Parallelism     uint8    `yaml:"argon2_parallelism"`
	MaxConcurrentHashes   int      `yaml:"max_concurrent_hashes"`
	PasswordBlocklistFile string   `yaml:"password_blocklist_file"`
}
type WebAuthn struct {
	RPID    string   `yaml:"rp_id"`
	RPName  string   `yaml:"rp_name"`
	Origins []string `yaml:"origins"`
}
type Limits struct {
	MaxWebSockets           int `yaml:"max_websockets"`
	MaxWebSocketsPerSession int `yaml:"max_websockets_per_session"`
	LiveQueuePackets        int `yaml:"live_queue_packets"`
	LiveQueueBytes          int `yaml:"live_queue_bytes"`
	PlaybackQueuePackets    int `yaml:"playback_queue_packets"`
	PlaybackQueueBytes      int `yaml:"playback_queue_bytes"`
	TransmitQueuePackets    int `yaml:"transmit_queue_packets"`
	ControlQueueMessages    int `yaml:"control_queue_messages"`
	ArchiveQueuePackets     int `yaml:"archive_queue_packets"`
	ArchiveQueueBytes       int `yaml:"archive_queue_bytes"`
	MaxPlaybacks            int `yaml:"max_playbacks"`
}
type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

func Defaults() Config {
	return Config{
		Web:            Web{"127.0.0.1:8090", "127.0.0.1:8091", "", true, []string{"127.0.0.0/8", "::1/128"}},
		Reflector:      Reflector{"127.0.0.1:46000", "http://127.0.0.1:8080", "WEB", Duration(time.Second), Duration(3 * time.Second), Duration(500 * time.Millisecond), Duration(30 * time.Second), "OPUSREF_SHARED_KEY", ""},
		Storage:        Storage{"/var/lib/opusrefweb/opusrefweb.db", "/var/lib/opusrefweb/recordings", Duration(24 * time.Hour), 10 * 1024 * 1024 * 1024, Duration(time.Minute), Duration(720 * time.Hour)},
		Authentication: Authentication{Duration(12 * time.Hour), Duration(168 * time.Hour), 3, 65536, 3, 4, 2, ""},
		WebAuthn:       WebAuthn{RPName: "OpusRef"},
		Limits:         Limits{250, 3, 64, 131072, 64, 131072, 64, 32, 512, 1048576, 50}, Logging: Logging{"info", "json"},
	}
}

func Load(path string) (Config, error) {
	c := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return Config{}, err
	}
	if err = c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Validate() error {
	if c.Web.HTTPListen == "" || c.Web.MonitorListen == "" {
		return errors.New("both web listeners are required")
	}
	for _, address := range []string{c.Web.HTTPListen, c.Web.MonitorListen} {
		if _, err := net.ResolveTCPAddr("tcp", address); err != nil {
			return fmt.Errorf("invalid listen address: %w", err)
		}
	}
	if !isLoopbackListen(c.Web.MonitorListen) {
		return errors.New("monitor listener must use loopback")
	}
	for _, value := range c.Web.TrustedProxyCIDRs {
		if _, _, err := net.ParseCIDR(value); err != nil {
			return errors.New("trusted proxy CIDR is invalid")
		}
	}
	if !isLoopbackListen(c.Web.HTTPListen) {
		u, err := url.Parse(c.Web.PublicOrigin)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("a non-loopback listener requires an HTTPS public origin")
		}
	}
	if _, err := wire.Callsign(c.Reflector.NodeCallsign); err != nil {
		return fmt.Errorf("invalid gateway node callsign: %w", err)
	}
	if c.Storage.SQLitePath == "" || c.Storage.ArchiveDirectory == "" || !filepath.IsAbs(c.Storage.SQLitePath) || !filepath.IsAbs(c.Storage.ArchiveDirectory) {
		return errors.New("storage paths must be absolute")
	}
	if c.Storage.Retention <= 0 || c.Storage.HardQuotaBytes <= 0 || c.Storage.PurgeInterval <= 0 || c.Storage.AuditRetention <= 0 {
		return errors.New("storage limits must be positive")
	}
	if c.Authentication.SessionIdle <= 0 || c.Authentication.SessionAbsolute < c.Authentication.SessionIdle || c.Authentication.MaxSessionsPerAccount < 1 {
		return errors.New("session limits are invalid")
	}
	if c.Authentication.Argon2MemoryKiB < 19*1024 || c.Authentication.Argon2Iterations < 2 || c.Authentication.Argon2Parallelism < 1 || c.Authentication.MaxConcurrentHashes < 1 {
		return errors.New("Argon2 parameters are below the permitted floor")
	}
	if (c.WebAuthn.RPID == "") != (len(c.WebAuthn.Origins) == 0) {
		return errors.New("WebAuthn RP ID and origins must be configured together")
	}
	if c.WebAuthn.RPID != "" {
		if net.ParseIP(c.WebAuthn.RPID) != nil {
			return errors.New("WebAuthn RP ID must not be an IP address")
		}
		u, err := url.Parse(c.Web.PublicOrigin)
		if err != nil || u.Scheme != "https" || !domainSuffix(u.Hostname(), c.WebAuthn.RPID) {
			return errors.New("WebAuthn RP ID is not valid for the public origin")
		}
		found := false
		for _, origin := range c.WebAuthn.Origins {
			if origin == c.Web.PublicOrigin {
				found = true
			}
		}
		if !found {
			return errors.New("WebAuthn origins must contain the exact public origin")
		}
	}
	for _, v := range []int{c.Limits.MaxWebSockets, c.Limits.MaxWebSocketsPerSession, c.Limits.LiveQueuePackets, c.Limits.LiveQueueBytes, c.Limits.PlaybackQueuePackets, c.Limits.PlaybackQueueBytes, c.Limits.TransmitQueuePackets, c.Limits.ControlQueueMessages, c.Limits.ArchiveQueuePackets, c.Limits.ArchiveQueueBytes, c.Limits.MaxPlaybacks} {
		if v <= 0 {
			return errors.New("all queue and connection limits must be positive")
		}
	}
	return nil
}
func (c Config) PasskeysEnabled() bool { return c.WebAuthn.RPID != "" && len(c.WebAuthn.Origins) > 0 }
func isLoopbackListen(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func domainSuffix(host, rp string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	rp = strings.ToLower(strings.TrimSuffix(rp, "."))
	return host == rp || strings.HasSuffix(host, "."+rp)
}
