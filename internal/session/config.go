// Package session gets two ferry peers talking: by a one-time code, or as
// paired contacts.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pion/webrtc/v4"

	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/link"
)

// Config is ferry's settings, from config.json and the environment.
type Config struct {
	// Rendezvous is the Concord rendezvous multiaddr.
	Rendezvous string `json:"rendezvous,omitempty"`
	// STUN servers; empty means the default.
	STUN []string `json:"stun,omitempty"`
	// Lanes is how many peer connections a transfer uses; 0 picks by size.
	Lanes int `json:"lanes,omitempty"`
	// Port fixes the UDP port connections use, for forwarding on a router;
	// 0 picks a free port per connection.
	Port int `json:"port,omitempty"`
	// NoBoxPorts turns off ferry shell's sharing of a lent box's ports.
	NoBoxPorts bool `json:"no_box_ports,omitempty"`
	// NoBoxInternet turns off ferry shell's carrying of a lent box's web
	// traffic (to public addresses only) through this machine.
	NoBoxInternet bool `json:"no_box_internet,omitempty"`

	// Not saved: test and debug switches.
	Loopback bool `json:"-"`
}

// DefaultRendezvous is used when neither config.json nor FERRY_RENDEZVOUS
// names one.
const DefaultRendezvous = "/dns/ferry-rdv-zahak.fly.dev/tcp/4001/p2p/12D3KooWG3abzGo1rA3azY76kU91omQxcJDTwwHTzdR1QqgwuJsz"

// DefaultSTUN is used when no STUN server is configured.
var DefaultSTUN = []string{"stun:stun.l.google.com:19302"}

// MaxLanes is the most lanes a transfer uses. See docs/throughput.md:
// on a long, lossy path 16 lanes nearly double what 8 achieve.
const MaxLanes = 16

// LanesFor picks how many lanes to use for a transfer of total bytes,
// unless the configuration fixes the number. Small transfers do not need
// many connections, and each one costs a handshake.
func (c *Config) LanesFor(total int64) int {
	if c.Lanes > 0 {
		return c.Lanes
	}
	n := int(total / (32 << 20))
	return max(2, min(n, MaxLanes))
}

func configPath() string { return filepath.Join(keys.Dir(), "config.json") }

// LoadConfig reads config.json, then applies FERRY_* environment overrides.
func LoadConfig() (*Config, error) {
	c, err := LoadFileConfig()
	if err != nil {
		return nil, err
	}
	c.applyEnv()
	return c, nil
}

// LoadFileConfig reads config.json alone, for editing it.
func LoadFileConfig() (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(configPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("%s is damaged: %w", configPath(), err)
		}
	}
	return c, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("FERRY_RENDEZVOUS"); v != "" {
		c.Rendezvous = v
	}
	if v := os.Getenv("FERRY_LANES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Lanes = n
		}
	}
	if v := os.Getenv("FERRY_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Port = n
		}
	}
	if v := os.Getenv("FERRY_STUN"); v != "" {
		c.STUN = strings.Split(v, ",")
	}
	if os.Getenv("FERRY_LOOPBACK") != "" {
		c.Loopback = true
	}
	if c.Lanes > 32 {
		c.Lanes = 32
	}
	if c.Rendezvous == "" {
		c.Rendezvous = DefaultRendezvous
	}
}

// Save writes config.json.
func (c *Config) Save() error {
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(keys.Dir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(configPath(), append(b, '\n'), 0o600)
}

// ErrNoRendezvous means no rendezvous is configured.
var ErrNoRendezvous = errors.New("no rendezvous configured")

func (c *Config) need() error {
	if c.Rendezvous == "" {
		return ErrNoRendezvous
	}
	return nil
}

// ICEServers returns the STUN servers. ferry uses no relay: data goes
// directly between the two machines or not at all.
func (c *Config) ICEServers() []webrtc.ICEServer {
	if c.Loopback {
		return nil
	}
	stun := c.STUN
	if len(stun) == 0 {
		stun = DefaultSTUN
	}
	return []webrtc.ICEServer{{URLs: stun}}
}

// LinkOptions builds link options for a session.
func (c *Config) LinkOptions(ctx context.Context, lanes int) link.Options {
	if lanes <= 0 {
		lanes = c.Lanes
	}
	return link.Options{
		Lanes:      lanes,
		ICEServers: c.ICEServers(),
		Loopback:   c.Loopback,
		Port:       c.Port,
	}
}
