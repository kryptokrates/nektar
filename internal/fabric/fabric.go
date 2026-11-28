package fabric

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"

	"github.com/lightwebinc/interest-relay/distributor"
	"github.com/lightwebinc/interest-relay/relay"
	"github.com/lightwebinc/shard-common/interest"
	"github.com/lightwebinc/shard-common/objfmt"

	"github.com/kryptokrates/nektar/internal/eventbus"
)

// TopicID computes the canonical 32-byte BRC-223 / BRC-148 TopicID = SHA-256(UTF-8 topic name).
func TopicID(topic string) interest.TopicID {
	return interest.TopicID(objfmt.TopicID(topic))
}

// EncodeDeliveryRecord encodes a BRC-149 submission & delivery record matching Lightweb's
// shard-common/objfmt + interest-relay/distributor wire contract.
func EncodeDeliveryRecord(topics []string, beef []byte) ([]byte, []interest.TopicID, error) {
	if len(topics) == 0 {
		return nil, nil, errors.New("fabric: at least one topic is required")
	}
	if len(beef) == 0 {
		return nil, nil, errors.New("fabric: empty BEEF object")
	}
	clean := make([]string, 0, len(topics))
	ids := make([]interest.TopicID, 0, len(topics))
	seen := make(map[interest.TopicID]bool, len(topics))
	for _, t := range topics {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		id := TopicID(t)
		if seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, t)
		ids = append(ids, id)
	}
	if len(clean) == 0 {
		return nil, nil, errors.New("fabric: no non-empty topics")
	}
	subRec, err := objfmt.EncodeBEEFRecord(clean, beef)
	if err != nil {
		return nil, nil, fmt.Errorf("fabric: encode BRC-149 BEEF record: %w", err)
	}
	return objfmt.EncodeBEEFDelivery(ids[0], subRec), ids, nil
}

// DecodeDeliveryRecord decodes one length-delimited BRC-149 delivery record from r, returning
// its primary TopicID, embedded topic names (when wrapped via objfmt.EncodeBEEFRecord), and raw BEEF bytes.
func DecodeDeliveryRecord(r io.Reader, maxRecord int) (interest.TopicID, []string, []byte, error) {
	var tid interest.TopicID
	if maxRecord <= 0 {
		maxRecord = distributor.DefaultMaxRecord
	}
	var hdr [objfmt.BEEFDeliveryHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return tid, nil, nil, err
	}
	copy(tid[:], hdr[:32])
	objLen := binary.BigEndian.Uint32(hdr[32:36])
	total := objfmt.BEEFDeliveryHeaderSize + int64(objLen)
	if objLen == 0 || total > int64(maxRecord) {
		return tid, nil, nil, fmt.Errorf("fabric: delivery record length %d out of bounds (max %d)", objLen, maxRecord)
	}
	payload := make([]byte, objLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return tid, nil, nil, err
	}
	if beef, names, err := objfmt.SplitBEEFPayload(payload); err == nil && len(beef) > 0 {
		return tid, names, beef, nil
	}
	return tid, nil, payload, nil
}

// HubConfig configures an embedded BRC-223 Topic Interest Receiver + BRC-149 Unicast Distributor on a Hub.
type HubConfig struct {
	UDPListen          string         // UDP address for BRC-223 interest datagrams (e.g. "127.0.0.1:0")
	AllowPrefixes      []netip.Prefix // allowed sender CIDR prefixes (defaults to 0.0.0.0/0 + ::/0 when empty)
	MaxTopicsPerSender int            // default 512
	MaxSenders         int            // default 4096
	Logger             *slog.Logger
	OnTopicInterest    func(topic interest.TopicID, live bool)
}

// HubInterestServer embeds Lightweb's BRC-223 `shard-common/interest.Receiver` and
// `interest-relay/distributor.Distributor` inside a Hub process, delivering admitted BEEFs
// in O(1) non-blocking TCP streams strictly to subscribers that declared interest in the topic.
type HubInterestServer struct {
	cfg      HubConfig
	log      *slog.Logger
	allow    []netip.Prefix
	rcv      *interest.Receiver
	dist     *distributor.Distributor
	udpConn  *net.UDPConn
	udpAddr  netip.AddrPort
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopOnce sync.Once
}

type hubInterestPolicy struct {
	allow []netip.Prefix
}

func (p *hubInterestPolicy) Identify(src netip.AddrPort) (string, bool) {
	a := src.Addr().Unmap()
	for _, pfx := range p.allow {
		if pfx.Contains(a) {
			return a.String(), true
		}
	}
	return "", false
}

func (p *hubInterestPolicy) Scope(string) *interest.Scope                      { return nil }
func (p *hubInterestPolicy) Bound(string, interest.TopicID) interest.Bound     { return interest.BoundFree }
func (p *hubInterestPolicy) Quota(string) int                                  { return -1 }
func (p *hubInterestPolicy) Note(string, interest.TopicID) interest.Status     { return interest.StatusOK }
func (p *hubInterestPolicy) Notes(string, func(interest.TopicID, interest.Status) bool) {}
func (p *hubInterestPolicy) Honor(_ string, src netip.AddrPort, _ interest.TopicID, d interest.Delivery) interest.Status {
	if d.Kind != interest.DeliverUnicast || d.Encoding != interest.EncodeRecords || d.Port == 0 {
		return interest.StatusAddressNotHonored
	}
	to, from := d.Addr.Unmap(), src.Addr().Unmap()
	if to == from || (from.IsLoopback() && to.IsLoopback()) {
		return interest.StatusOK
	}
	for _, pfx := range p.allow {
		if pfx.Contains(from) && pfx.Contains(to) {
			return interest.StatusOK
		}
	}
	return interest.StatusBadAddress
}

// StartHubInterestServer binds a UDP BRC-223 interest socket and starts the non-blocking BRC-149 distributor.
func StartHubInterestServer(parentCtx context.Context, cfg HubConfig) (*HubInterestServer, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	allow := cfg.AllowPrefixes
	if len(allow) == 0 {
		allow = []netip.Prefix{
			netip.MustParsePrefix("0.0.0.0/0"),
			netip.MustParsePrefix("::/0"),
		}
	}
	if cfg.MaxTopicsPerSender <= 0 {
		cfg.MaxTopicsPerSender = 512
	}
	if cfg.MaxSenders <= 0 {
		cfg.MaxSenders = 4096
	}
	listenAddr := strings.TrimSpace(cfg.UDPListen)
	if listenAddr == "" {
		listenAddr = "127.0.0.1:0"
	}
	udpResolved, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("fabric: resolve UDP %s: %w", listenAddr, err)
	}
	udpConn, err := net.ListenUDP("udp", udpResolved)
	if err != nil {
		return nil, fmt.Errorf("fabric: listen UDP %s: %w", listenAddr, err)
	}
	ap, err := netip.ParseAddrPort(udpConn.LocalAddr().String())
	if err != nil {
		_ = udpConn.Close()
		return nil, err
	}

	dist := distributor.New(distributor.Config{
		Listen: "127.0.0.1:0",
		Logger: cfg.Logger,
	})
	pol := &hubInterestPolicy{allow: allow}
	rcv := interest.NewReceiver(interest.ReceiverConfig{
		Policy:             pol,
		OnDelivery:         dist.Route,
		Lifetimes:          interest.DefaultLifetimes,
		MaxTopicsPerSender: cfg.MaxTopicsPerSender,
		MaxSenders:         cfg.MaxSenders,
		OnChange: func(_ string, t interest.TopicID, live bool) {
			if cfg.OnTopicInterest != nil {
				cfg.OnTopicInterest(t, live)
			}
		},
	})

	ctx, cancel := context.WithCancel(parentCtx)
	s := &HubInterestServer{
		cfg:     cfg,
		log:     cfg.Logger,
		allow:   allow,
		rcv:     rcv,
		dist:    dist,
		udpConn: udpConn,
		udpAddr: ap,
		cancel:  cancel,
	}
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		_ = interest.ServeReceiver(ctx, udpConn, rcv, interest.ServeOptions{})
	}()
	go func() {
		defer s.wg.Done()
		_ = dist.Serve(ctx)
	}()
	return s, nil
}

// UDPAddr returns the bound BRC-223 UDP endpoint that downstream agents & relays announce interest to.
func (s *HubInterestServer) UDPAddr() netip.AddrPort { return s.udpAddr }

// DistributorStats returns live routing & delivery telemetry from the embedded BRC-149 distributor.
func (s *HubInterestServer) DistributorStats() distributor.Stats { return s.dist.Stats() }

// Fanout encodes a BRC-149 delivery record once and copies it non-blockingly to every downstream host
// routed for any of `topics` (excluding `skip`).
func (s *HubInterestServer) Fanout(topics []string, beef []byte, skip netip.Addr) int {
	if s == nil || s.dist == nil || len(topics) == 0 || len(beef) == 0 {
		return 0
	}
	drec, ids, err := EncodeDeliveryRecord(topics, beef)
	if err != nil {
		return 0
	}
	return s.dist.Copy(ids, drec, skip)
}

// Close shuts down the BRC-223 UDP receiver and BRC-149 TCP distributor cleanly.
func (s *HubInterestServer) Close() error {
	if s == nil {
		return nil
	}
	var closeErr error
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.udpConn != nil {
			closeErr = s.udpConn.Close()
		}
		s.wg.Wait()
	})
	return closeErr
}

// DeliveryEvent represents one verified BRC-149 BEEF object pushed over the BRC-223 interest plane.
type DeliveryEvent struct {
	TopicID interest.TopicID
	Topic   string
	Topics  []string
	BEEF    []byte
}

// SubscriberConfig configures an Agent or Leaf Hub BRC-223 topic interest announcer + BRC-149 TCP receiver.
type SubscriberConfig struct {
	Label          string // stable node label passed to interest.NewSenderID
	Upstreams      []netip.AddrPort
	DeliveryListen string // local TCP address to receive BRC-149 records (default "127.0.0.1:0")
	AdvertiseAddr  netip.Addr
	Bus            *eventbus.Bus
	OnObject       func(ctx context.Context, ev DeliveryEvent)
	Logger         *slog.Logger
}

// Subscriber announces dynamic topic interest (`tm_nektar_<net>_dom_<slug>`, `tm_nektar_<net>_ont_<id16>`,
// `tm_bbox_<office>`, `tm_finger`, `tm_bstore_<bucket>`) upstream via BRC-223 UDP datagrams and receives
// pushed BRC-149 BEEF records over TCP with zero polling.
type Subscriber struct {
	cfg         SubscriberConfig
	log         *slog.Logger
	cli         *interest.Client
	udpConn     *net.UDPConn
	tcpLn       net.Listener
	deliverAddr netip.AddrPort
	deliveryExt []byte

	mu        sync.RWMutex
	topicByID map[interest.TopicID]string
	wanted    map[interest.TopicID]bool

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// StartSubscriber starts a BRC-223 `interest.Client` + TCP BRC-149 listener with zero polling.
func StartSubscriber(parentCtx context.Context, cfg SubscriberConfig) (*Subscriber, error) {
	if len(cfg.Upstreams) == 0 {
		return nil, errors.New("fabric: at least one upstream BRC-223 AddrPort is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	dlListen := strings.TrimSpace(cfg.DeliveryListen)
	if dlListen == "" {
		dlListen = "127.0.0.1:0"
	}
	tcpLn, err := net.Listen("tcp", dlListen)
	if err != nil {
		return nil, fmt.Errorf("fabric: listen delivery TCP %s: %w", dlListen, err)
	}
	tcpAP, err := netip.ParseAddrPort(tcpLn.Addr().String())
	if err != nil {
		_ = tcpLn.Close()
		return nil, err
	}
	advAddr := cfg.AdvertiseAddr
	if !advAddr.IsValid() || advAddr.IsUnspecified() {
		advAddr = tcpAP.Addr()
		if advAddr.IsUnspecified() {
			advAddr = netip.MustParseAddr("127.0.0.1")
		}
	}
	deliverAP := netip.AddrPortFrom(advAddr.Unmap(), tcpAP.Port())
	dl := interest.Delivery{
		Kind:     interest.DeliverUnicast,
		Encoding: interest.EncodeRecords,
		Addr:     deliverAP.Addr(),
		Port:     deliverAP.Port(),
	}
	dlVal, err := dl.AppendValue(nil)
	if err != nil {
		_ = tcpLn.Close()
		return nil, err
	}
	ext := interest.AppendTLV(nil, interest.ExtDelivery, dlVal)

	udpBind := "127.0.0.1:0"
	if !cfg.Upstreams[0].Addr().IsLoopback() {
		udpBind = ":0"
	}
	udpResolved, err := net.ResolveUDPAddr("udp", udpBind)
	if err != nil {
		_ = tcpLn.Close()
		return nil, err
	}
	udpConn, err := net.ListenUDP("udp", udpResolved)
	if err != nil {
		_ = tcpLn.Close()
		return nil, err
	}

	label := strings.TrimSpace(cfg.Label)
	if label == "" {
		label = fmt.Sprintf("nektar-%s", deliverAP.String())
	}
	cli := interest.NewClient(interest.ClientConfig{
		SenderID:  interest.NewSenderID(label),
		Upstreams: cfg.Upstreams,
	})

	ctx, cancel := context.WithCancel(parentCtx)
	sub := &Subscriber{
		cfg:         cfg,
		log:         cfg.Logger,
		cli:         cli,
		udpConn:     udpConn,
		tcpLn:       tcpLn,
		deliverAddr: deliverAP,
		deliveryExt: ext,
		topicByID:   make(map[interest.TopicID]string),
		wanted:      make(map[interest.TopicID]bool),
		cancel:      cancel,
	}
	sub.wg.Add(2)
	go func() {
		defer sub.wg.Done()
		_ = interest.RunClient(ctx, udpConn, cli, interest.RunOptions{WithdrawOnExit: true})
	}()
	go func() {
		defer sub.wg.Done()
		sub.acceptLoop(ctx)
	}()
	return sub, nil
}

// DeliveryAddr returns the local TCP `AddrPort` advertised in BRC-223 `ExtDelivery`.
func (s *Subscriber) DeliveryAddr() netip.AddrPort { return s.deliverAddr }

// Client returns the underlying BRC-223 `interest.Client` for status/sync telemetry.
func (s *Subscriber) Client() *interest.Client { return s.cli }

// SetTopics updates the exact set of dynamic overlay topics this node wants delivered.
func (s *Subscriber) SetTopics(topics []string) {
	next := make(map[interest.TopicID]string, len(topics))
	for _, t := range topics {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		next[TopicID(t)] = t
	}
	s.mu.Lock()
	var drop []interest.TopicID
	for id := range s.wanted {
		if _, ok := next[id]; !ok {
			drop = append(drop, id)
			delete(s.wanted, id)
		}
	}
	var add []interest.TopicID
	for id, name := range next {
		s.topicByID[id] = name
		if !s.wanted[id] {
			s.wanted[id] = true
			add = append(add, id)
		}
	}
	ext := append([]byte(nil), s.deliveryExt...)
	s.mu.Unlock()

	for _, id := range drop {
		s.cli.Remove(id)
	}
	for _, id := range add {
		s.cli.Set(id, ext)
	}
}

func (s *Subscriber) acceptLoop(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() { _ = s.tcpLn.Close() })
	defer stop()
	var connWG sync.WaitGroup
	defer connWG.Wait()
	for {
		c, err := s.tcpLn.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		connWG.Add(1)
		go func(conn net.Conn) {
			defer connWG.Done()
			s.readLaneConn(ctx, conn)
		}(c)
	}
}

func (s *Subscriber) readLaneConn(ctx context.Context, c net.Conn) {
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer func() { _ = c.Close() }()
	br := bufio.NewReaderSize(c, 64<<10)
	for {
		tid, names, beef, err := DecodeDeliveryRecord(br, distributor.DefaultMaxRecord)
		if err != nil {
			return
		}
		s.mu.RLock()
		primary := s.topicByID[tid]
		if primary == "" && len(names) > 0 {
			primary = names[0]
		}
		s.mu.RUnlock()
		ev := DeliveryEvent{
			TopicID: tid,
			Topic:   primary,
			Topics:  names,
			BEEF:    beef,
		}
		if s.cfg.OnObject != nil {
			s.cfg.OnObject(ctx, ev)
		}
		if s.cfg.Bus != nil {
			busTopic := "fabric:object"
			if primary != "" {
				busTopic = "fabric:" + primary
			}
			s.cfg.Bus.Publish(busTopic, "delivery", ev)
		}
	}
}

// Close withdraws BRC-223 topic interest and closes all UDP/TCP sockets cleanly.
func (s *Subscriber) Close() error {
	if s == nil {
		return nil
	}
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.tcpLn != nil {
			_ = s.tcpLn.Close()
		}
		s.wg.Wait()
		if s.udpConn != nil {
			_ = s.udpConn.Close()
		}
	})
	return nil
}

// NewEmbeddedRelay constructs a multi-hop BRC-223 `interest-relay/relay.Relay` so edge gateways can
// aggregate downstream interest and distribute BRC-149 lanes without external binaries.
func NewEmbeddedRelay(cfg relay.Config) (*relay.Relay, error) {
	return relay.New(cfg)
}
