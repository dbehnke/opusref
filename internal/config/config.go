// Package config contains the future server configuration model.
package config

import "time"

// Config is the configuration shape documented by config.example.yaml.
type Config struct {
	Network        Network
	Reflector      Reflector
	Limits         Limits
	Timers         Timers
	Authentication Authentication
	Monitoring     Monitoring
	Logging        Logging
}

type Network struct{ UDPListen string }
type Reflector struct{ ID, DisplayName string }
type Limits struct{ MaxClients, MaxDatagramBytes, OutboundQueueFrames, RecentEvents int }
type Timers struct {
	KeepaliveInterval, SessionTimeout, GrantTimeout time.Duration
	StreamInactivityTimeout, TransmitTimeout        time.Duration
}
type Authentication struct{ SharedKeyEnv, SharedKeyFile string }
type Monitoring struct{ HTTPListen string }
type Logging struct{ Level, Format string }
