// Package rv talks to a Concord rendezvous node.
//
// ferry uses three things the node already offers, and adds nothing to it:
//
//   - the DHT, to find the other side of a pairing code by its slot number;
//   - circuit relay, so two peers behind NAT can open a libp2p stream to each
//     other for signaling (Noise runs end to end, the node only forwards);
//   - the mailbox, to leave sealed messages for a contact who is offline.
//
// The node is not trusted with content: everything ferry sends through it is
// sealed under keys the node never sees.
package rv

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ipfs/go-cid"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"
	mh "github.com/multiformats/go-multihash"
)

// Node is a connection to one rendezvous.
type Node struct {
	Host host.Host
	RV   peer.AddrInfo
	dht  *dht.IpfsDHT
}

// PeerKey derives the libp2p key for an identity. It is separate from the
// signing key so that nothing signed for libp2p can be replayed as a ferry
// signature, and the reverse.
func PeerKey(signKey ed25519.PrivateKey) (crypto.PrivKey, error) {
	h := sha256.Sum256(append([]byte("ferry libp2p key v1\x00"), signKey.Seed()...))
	return crypto.UnmarshalEd25519PrivateKey(ed25519.NewKeyFromSeed(h[:]))
}

// PeerID returns the libp2p peer ID for an identity's signing key.
func PeerID(signKey ed25519.PrivateKey) (peer.ID, error) {
	sk, err := PeerKey(signKey)
	if err != nil {
		return "", err
	}
	return peer.IDFromPrivateKey(sk)
}

// Dial connects to the rendezvous at addr (a multiaddr ending in /p2p/<id>).
// key may be nil for a throwaway identity.
func Dial(ctx context.Context, addr string, key crypto.PrivKey) (*Node, error) {
	m, err := ma.NewMultiaddr(addr)
	if err != nil {
		return nil, fmt.Errorf("rendezvous address %q: %w", addr, err)
	}
	info, err := peer.AddrInfoFromP2pAddr(m)
	if err != nil {
		return nil, fmt.Errorf("rendezvous address %q must end in /p2p/<peer id>: %w", addr, err)
	}
	opts := []libp2p.Option{
		libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0", "/ip4/0.0.0.0/udp/0/quic-v1"),
		libp2p.EnableRelay(),
		libp2p.DisableMetrics(),
	}
	if key != nil {
		opts = append(opts, libp2p.Identity(key))
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, err
	}
	if err := h.Connect(ctx, *info); err != nil {
		h.Close()
		return nil, fmt.Errorf("cannot reach the rendezvous at %s: %w", addr, err)
	}
	return &Node{Host: h, RV: *info}, nil
}

// Close disconnects.
func (n *Node) Close() error {
	if n.dht != nil {
		n.dht.Close()
	}
	return n.Host.Close()
}

// Reserve asks the rendezvous to relay incoming streams to us, and keeps the
// reservation fresh until ctx ends.
func (n *Node) Reserve(ctx context.Context) error {
	r, err := client.Reserve(ctx, n.Host, n.RV)
	if err != nil {
		return fmt.Errorf("rendezvous refused a relay slot: %w", err)
	}
	go func() {
		for {
			wait := time.Until(r.Expiration) / 2
			if wait < 10*time.Second {
				wait = 10 * time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if nr, err := client.Reserve(ctx, n.Host, n.RV); err == nil {
				r = nr
			}
		}
	}()
	return nil
}

// Open opens a stream to a peer through the rendezvous relay.
func (n *Node) Open(ctx context.Context, p peer.ID, proto protocol.ID) (network.Stream, error) {
	circ, err := ma.NewMultiaddr("/p2p/" + n.RV.ID.String() + "/p2p-circuit")
	if err != nil {
		return nil, err
	}
	var addrs []ma.Multiaddr
	for _, a := range n.RV.Addrs {
		addrs = append(addrs, a.Encapsulate(circ))
	}
	n.Host.Peerstore().AddAddrs(p, addrs, time.Hour)
	ctx = network.WithAllowLimitedConn(ctx, "ferry")
	if err := n.Host.Connect(ctx, peer.AddrInfo{ID: p, Addrs: addrs}); err != nil {
		return nil, err
	}
	return n.Host.NewStream(ctx, p, proto)
}

// Handle serves incoming streams for proto.
func (n *Node) Handle(proto protocol.ID, f func(network.Stream)) {
	n.Host.SetStreamHandler(proto, f)
}

func (n *Node) routing(ctx context.Context) (*dht.IpfsDHT, error) {
	if n.dht != nil {
		return n.dht, nil
	}
	d, err := dht.New(n.Host, dht.Mode(dht.ModeClient), dht.BootstrapPeers(n.RV))
	if err != nil {
		return nil, err
	}
	// Our only DHT server is the rendezvous; make sure it is in the table.
	if _, err := d.RoutingTable().TryAddPeer(n.RV.ID, true, false); err != nil {
		d.Close()
		return nil, err
	}
	n.dht = d
	return d, nil
}

func slotCID(slot string) cid.Cid {
	sum, _ := mh.Sum([]byte("ferry slot v1\x00"+slot), mh.SHA2_256, -1)
	return cid.NewCidV1(cid.Raw, sum)
}

// Announce publishes that we are waiting on a pairing slot.
// The slot is only a meeting point; it carries none of the secret words.
func (n *Node) Announce(ctx context.Context, slot string) error {
	d, err := n.routing(ctx)
	if err != nil {
		return err
	}
	return d.Provide(ctx, slotCID(slot), true)
}

// Lookup finds peers announcing a slot, waiting up to ctx.
func (n *Node) Lookup(ctx context.Context, slot string) ([]peer.ID, error) {
	d, err := n.routing(ctx)
	if err != nil {
		return nil, err
	}
	for {
		var out []peer.ID
		for ai := range d.FindProvidersAsync(ctx, slotCID(slot), 8) {
			if ai.ID != n.Host.ID() {
				out = append(out, ai.ID)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ---- mailbox, wire-compatible with /concord/mailbox/1.0.0 ----

const mailboxProto protocol.ID = "/concord/mailbox/1.0.0"

// MailboxID is the address of a peer's mailbox on the rendezvous.
func MailboxID(p peer.ID) (string, error) {
	pk, err := p.ExtractPublicKey()
	if err != nil {
		return "", err
	}
	raw, err := pk.Raw()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(raw)
	h.Write([]byte("concord-mbx-v1"))
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

type mboxReq struct {
	Op         string   `json:"op"`
	Target     string   `json:"target,omitempty"`
	Envelope   []byte   `json:"envelope,omitempty"`
	TTLSeconds int64    `json:"ttl,omitempty"`
	AckIDs     []string `json:"ackIds,omitempty"`
}

// Envelope is one stored message.
type Envelope struct {
	ID   string `json:"id"`
	Data []byte `json:"data"`
}

type mboxResp struct {
	OK        bool       `json:"ok"`
	Envelopes []Envelope `json:"envelopes,omitempty"`
	Error     string     `json:"error,omitempty"`
}

func (n *Node) mailbox(ctx context.Context, req mboxReq) (mboxResp, error) {
	s, err := n.Host.NewStream(network.WithAllowLimitedConn(ctx, "mailbox"), n.RV.ID, mailboxProto)
	if err != nil {
		return mboxResp{}, err
	}
	defer s.Close()
	if dl, ok := ctx.Deadline(); ok {
		s.SetDeadline(dl)
	}
	body, _ := json.Marshal(req)
	if err := WriteFrame(s, body); err != nil {
		return mboxResp{}, err
	}
	if err := s.CloseWrite(); err != nil {
		return mboxResp{}, err
	}
	b, err := ReadFrame(s, 1<<20)
	if err != nil {
		return mboxResp{}, err
	}
	var resp mboxResp
	if err := json.Unmarshal(b, &resp); err != nil {
		return mboxResp{}, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("mailbox: %s", resp.Error)
	}
	return resp, nil
}

// Register opens our own mailbox so contacts can leave messages in it.
func (n *Node) Register(ctx context.Context) error {
	_, err := n.mailbox(ctx, mboxReq{Op: "register"})
	return err
}

// Deposit leaves a sealed envelope for a peer.
func (n *Node) Deposit(ctx context.Context, to peer.ID, data []byte, ttl time.Duration) error {
	id, err := MailboxID(to)
	if err != nil {
		return err
	}
	_, err = n.mailbox(ctx, mboxReq{Op: "deposit", Target: id, Envelope: data, TTLSeconds: int64(ttl.Seconds())})
	return err
}

// Drain fetches everything waiting in our mailbox. Call Ack once it is stored.
func (n *Node) Drain(ctx context.Context) ([]Envelope, error) {
	r, err := n.mailbox(ctx, mboxReq{Op: "drain"})
	return r.Envelopes, err
}

// Ack deletes envelopes we have stored.
func (n *Node) Ack(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := n.mailbox(ctx, mboxReq{Op: "ack", AckIDs: ids})
	return err
}

// ---- framing ----

// ErrTooBig means a frame was larger than the reader allows.
var ErrTooBig = errors.New("frame too large")

// WriteFrame writes a 4-byte big-endian length and the data.
func WriteFrame(w io.Writer, data []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// ReadFrame reads one frame of at most max bytes.
func ReadFrame(r io.Reader, max int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if int64(n) > int64(max) {
		return nil, ErrTooBig
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
