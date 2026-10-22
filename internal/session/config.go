// Package session gets two ferry peers talking: by a one-time code, or as
// paired contacts.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/pion/webrtc/v4"

	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/link"
)

// Config is ferry's settings, from config.json and the environment.
type Config struct {
	// Rendezvous is the Concord rendezvous multiaddr.
	Rendezvous string `json:"rendezvous,omitempty"`
	// RendezvousHTTP is its HTTP gateway, for TURN credentials.
	RendezvousHTTP string `json:"rendezvous_http,omitempty"`
	// STUN servers; empty means the default.
	STUN []string `json:"stun,omitempty"`
	// Lanes is how many peer connections a transfer uses.
	Lanes int `json:"lanes,omitempty"`

	// Not saved: test and debug switches.
	RelayOnly bool `json:"-"`
	Loopback  bool `json:"-"`
}

// DefaultSTUN is used when no STUN server is configured.
var DefaultSTUN = []string{"stun:stun.l.google.com:19302"}

// DefaultLanes is the number of lanes for a transfer. See docs/throughput.md.
const DefaultLanes = 8

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
	if v := os.Getenv("FERRY_RENDEZVOUS_HTTP"); v != "" {
		c.RendezvousHTTP = v
	}
	if v := os.Getenv("FERRY_LANES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Lanes = n
		}
	}
	if os.Getenv("FERRY_RELAY_ONLY") != "" {
		c.RelayOnly = true
	}
	if os.Getenv("FERRY_LOOPBACK") != "" {
		c.Loopback = true
	}
	if c.Lanes <= 0 {
		c.Lanes = DefaultLanes
	}
	if c.Lanes > 32 {
		c.Lanes = 32
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

// httpBase guesses the HTTP gateway from the multiaddr when not set.
func (c *Config) httpBase() string {
	if c.RendezvousHTTP != "" {
		return c.RendezvousHTTP
	}
	m, err := ma.NewMultiaddr(c.Rendezvous)
	if err != nil {
		return ""
	}
	for _, p := range []int{ma.P_DNS, ma.P_DNS4, ma.P_DNS6, ma.P_IP4, ma.P_IP6} {
		if v, err := m.ValueForProtocol(p); err == nil {
			if p == ma.P_IP6 {
				v = "[" + v + "]"
			}
			return "http://" + v + ":8080"
		}
	}
	return ""
}

// ICEServers returns STUN plus, if the rendezvous offers it, TURN.
func (c *Config) ICEServers(ctx context.Context) []webrtc.ICEServer {
	stun := c.STUN
	if len(stun) == 0 {
		stun = DefaultSTUN
	}
	var out []webrtc.ICEServer
	if !c.Loopback {
		out = append(out, webrtc.ICEServer{URLs: stun})
	}
	base := c.httpBase()
	if base == "" {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/turn", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out
	}
	var r struct {
		IceServers []struct {
			URLs       []string `json:"urls"`
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
		} `json:"iceServers"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return out
	}
	for _, s := range r.IceServers {
		if s.Username == "" {
			continue // its STUN entry; we have our own
		}
		out = append(out, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	return out
}

// LinkOptions builds link options for a session.
func (c *Config) LinkOptions(ctx context.Context, lanes int) link.Options {
	if lanes <= 0 {
		lanes = c.Lanes
	}
	return link.Options{
		Lanes:      lanes,
		ICEServers: c.ICEServers(ctx),
		RelayOnly:  c.RelayOnly,
		Loopback:   c.Loopback,
	}
}
