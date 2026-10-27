// Package keys holds this machine's long-term identity and the contacts it
// has paired with.
//
// An identity is two keys: Ed25519 for signing messages, X25519 for agreeing
// a pairwise secret with each contact. Both live in one file, readable only
// by the owner.
package keys

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ZahakJ/ferry/internal/rv"
)

// Public is what a peer learns about us when we pair.
type Public struct {
	Name string `json:"name"`
	Sign []byte `json:"sign"` // Ed25519 public key
	Box  []byte `json:"box"`  // X25519 public key
	// Peer is the libp2p peer ID this identity answers on at the rendezvous.
	Peer string `json:"peer,omitempty"`
}

// ID is a short, stable fingerprint of a public identity.
func (p Public) ID() string {
	h := sha256.Sum256(append(append([]byte("ferry id\x00"), p.Sign...), p.Box...))
	return base64.RawURLEncoding.EncodeToString(h[:9])
}

// Identity is this machine's secret key pair.
type Identity struct {
	Name    string             `json:"name"`
	SignKey ed25519.PrivateKey `json:"sign_key"`
	BoxKey  []byte             `json:"box_key"`
	Created time.Time          `json:"created"`
}

// Public returns the shareable half of the identity.
func (id *Identity) Public() Public {
	bk, _ := ecdh.X25519().NewPrivateKey(id.BoxKey)
	p, _ := rv.PeerID(id.SignKey)
	return Public{
		Name: id.Name,
		Sign: id.SignKey.Public().(ed25519.PublicKey),
		Box:  bk.PublicKey().Bytes(),
		Peer: p.String(),
	}
}

// Shared returns the X25519 shared secret with a peer.
func (id *Identity) Shared(peer Public) ([]byte, error) {
	priv, err := ecdh.X25519().NewPrivateKey(id.BoxKey)
	if err != nil {
		return nil, err
	}
	pub, err := ecdh.X25519().NewPublicKey(peer.Box)
	if err != nil {
		return nil, err
	}
	return priv.ECDH(pub)
}

// New makes a fresh identity.
func New(name string) (*Identity, error) {
	_, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	bk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{Name: name, SignKey: sk, BoxKey: bk.Bytes(), Created: time.Now().UTC()}, nil
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// CheckName reports whether s can be used as a contact or machine name.
func CheckName(s string) error {
	if !nameRE.MatchString(s) {
		return fmt.Errorf("%q is not a usable name: use 1-32 lowercase letters, digits, - or _", s)
	}
	return nil
}

// DefaultName guesses a name for this machine from the user and host names.
func DefaultName() string {
	u := strings.ToLower(os.Getenv("USER"))
	if u == "" {
		u = "me"
	}
	s := regexp.MustCompile(`[^a-z0-9_-]`).ReplaceAllString(u, "")
	if s == "" || CheckName(s) != nil {
		return "me"
	}
	return s
}

// Dir is where ferry keeps its state. FERRY_HOME overrides it.
func Dir() string {
	if d := os.Getenv("FERRY_HOME"); d != "" {
		return d
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "ferry")
	}
	return filepath.Join(os.Getenv("HOME"), ".ferry")
}

func identityPath() string { return filepath.Join(Dir(), "identity.json") }
func contactsPath() string { return filepath.Join(Dir(), "contacts.json") }

// ErrNoIdentity means ferry has not been set up on this machine yet.
var ErrNoIdentity = errors.New("no identity yet")

// Load reads the identity, or returns ErrNoIdentity.
func Load() (*Identity, error) {
	b, err := os.ReadFile(identityPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoIdentity
	}
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", identityPath(), err)
	}
	if len(id.SignKey) != ed25519.PrivateKeySize || len(id.BoxKey) != 32 {
		return nil, fmt.Errorf("%s is damaged: bad key length", identityPath())
	}
	return &id, nil
}

// LoadOrCreate reads the identity, making one on first use.
// created reports whether it was just made.
func LoadOrCreate() (id *Identity, created bool, err error) {
	id, err = Load()
	if !errors.Is(err, ErrNoIdentity) {
		return id, false, err
	}
	id, err = New(DefaultName())
	if err != nil {
		return nil, false, err
	}
	return id, true, Save(id)
}

// Save writes the identity with owner-only permissions.
func Save(id *Identity) error {
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(identityPath(), b)
}

// Contact is a paired peer.
type Contact struct {
	Name   string    `json:"name"` // our local name for them
	Key    Public    `json:"key"`
	Paired time.Time `json:"paired"`
	// Kind is "" for someone else's machine (a friend), or KindMine for
	// another device of this machine's owner (ferry mine), which gets full
	// access to this machine. Only ferry mine add/join set it.
	Kind string `json:"kind,omitempty"`
	// Device describes a KindMine contact: how to reach its mesh sshd.
	Device *Device `json:"device,omitempty"`
}

// KindMine marks a contact as one of this owner's own devices.
const KindMine = "mine"

// Mine reports whether c is one of this owner's own devices.
func (c Contact) Mine() bool { return c.Kind == KindMine && c.Device != nil }

// Device is what one of your own devices tells the others about itself
// when they link (ferry mine): enough for ssh to reach it and for paths to
// map between homes.
type Device struct {
	User    string `json:"user"`     // its account
	Home    string `json:"home"`     // its home directory
	OS      string `json:"os"`       // linux | darwin
	SSHKey  string `json:"ssh_key"`  // its mesh client key (authorized_keys line, no options)
	HostKey string `json:"host_key"` // its mesh sshd's host key
}

// Contacts is the address book, keyed by local name.
type Contacts map[string]Contact

// LoadContacts reads the address book. A missing file is an empty book.
func LoadContacts() (Contacts, error) {
	b, err := os.ReadFile(contactsPath())
	if errors.Is(err, os.ErrNotExist) {
		return Contacts{}, nil
	}
	if err != nil {
		return nil, err
	}
	c := Contacts{}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", contactsPath(), err)
	}
	return c, nil
}

// Save writes the address book.
func (c Contacts) Save() error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(contactsPath(), b)
}

// Names returns contact names in order.
func (c Contacts) Names() []string {
	out := make([]string, 0, len(c))
	for n := range c {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ByKey finds the contact holding a public key.
func (c Contacts) ByKey(sign []byte) (Contact, bool) {
	for _, k := range c {
		if string(k.Key.Sign) == string(sign) {
			return k, true
		}
	}
	return Contact{}, false
}

// FreeName returns want, or want-2, want-3... if it is taken by another key.
func (c Contacts) FreeName(want string, key Public) string {
	if CheckName(want) != nil {
		want = "friend"
	}
	n := want
	for i := 2; ; i++ {
		k, taken := c[n]
		if !taken || string(k.Key.Sign) == string(key.Sign) {
			return n
		}
		n = fmt.Sprintf("%s-%d", want, i)
	}
}

// writeFile replaces path atomically, readable only by the owner.
func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
