package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/florianl/go-nfqueue"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

// Mark-based lazy ClusterIP load balancing (see research/services.md, the v2
// data plane). At boot we install ONE static rule set:
//
//	ip6 daddr <SVCCIDR> meta mark != 0 dnat ip6 to meta mark map @eps  # re-traversal
//	ip6 daddr <SVCCIDR> meta mark 0    queue num 0                     # first packet
//	map eps { type mark : ipv6_addr . inet_service }                  # id -> addr:port
//
// The first packet of a new flow (mark 0) pops up here; we resolve endpoints
// (host agent over vsock, cached), pick one in userspace, ensure it has an
// id + eps map element, and NF_REPEAT the packet with that mark set on the
// verdict. The re-traversed packet matches `meta mark != 0`, is DNAT'd via
// the map, and conntrack carries the rest of the flow in-kernel. No per-flow
// nftables operations: the map changes only when endpoints change.

const (
	svcVsockPort = 1025
	nfQueueNum   = 0
	svcTable     = "kube"
	epsMapName   = "eps"
	cacheTTL     = 10 * time.Second
)

type svcQuery struct {
	VIP   string `json:"vip"`
	Port  int    `json:"port"`
	Proto string `json:"proto"`
}

type svcAnswer struct {
	Endpoints  []string `json:"endpoints"`
	TargetPort int      `json:"targetPort"`
	TTLSeconds int      `json:"ttl"`
	Error      string   `json:"error,omitempty"`
}

type endpoint struct {
	addr string
	port int
}

type svcCacheEntry struct {
	endpoints []endpoint
	expiry    time.Time
	rr        int // round-robin cursor
}

type svcLB struct {
	cidr  *net.IPNet
	nft   *nftables.Conn
	table *nftables.Table
	chain *nftables.Chain
	eps   *nftables.Set // map: mark -> addr:port

	mu      sync.Mutex
	epID    map[endpoint]uint32 // endpoint -> mark id (>=1)
	nextID  uint32
	cache   map[string]*svcCacheEntry // "vip:port:proto" -> endpoints
	queryFn func(svcQuery) (svcAnswer, error)
}

func setupServiceLB(cidr string) error {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("parsing service cidr: %w", err)
	}
	lb := &svcLB{
		cidr:    ipnet,
		nft:     &nftables.Conn{},
		epID:    map[endpoint]uint32{},
		nextID:  1, // 0 is reserved (unmarked = pop up)
		cache:   map[string]*svcCacheEntry{},
		queryFn: querySvcOverVsock,
	}
	if err := lb.installBase(); err != nil {
		return err
	}
	nf, err := nfqueue.Open(&nfqueue.Config{
		NfQueue:      nfQueueNum,
		MaxPacketLen: 0xffff,
		MaxQueueLen:  0xff,
		Copymode:     nfqueue.NfQnlCopyPacket,
		WriteTimeout: 15 * time.Millisecond,
	})
	if err != nil {
		return fmt.Errorf("opening nfqueue: %w", err)
	}
	return lb.serve(nf)
}

func (lb *svcLB) installBase() error {
	lb.table = lb.nft.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv6, Name: svcTable})
	// map eps: mark -> (ipv6_addr . inet_service). Concatenation is false —
	// that flag is for concatenated KEYS; here only the DATA is a concat.
	lb.eps = &nftables.Set{
		Table:    lb.table,
		Name:     epsMapName,
		IsMap:    true,
		KeyType:  nftables.TypeMark,
		DataType: nftables.MustConcatSetType(nftables.TypeIP6Addr, nftables.TypeInetService),
	}
	if err := lb.nft.AddSet(lb.eps, nil); err != nil {
		return fmt.Errorf("add eps map: %w", err)
	}
	lb.chain = lb.nft.AddChain(&nftables.Chain{
		Name:     "output",
		Table:    lb.table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityNATDest,
	})
	// rule 1: ip6 daddr <cidr> meta mark != 0 dnat ip6 to meta mark map @eps
	lb.nft.AddRule(&nftables.Rule{
		Table: lb.table, Chain: lb.chain,
		Exprs: append(lb.matchCIDR(),
			&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
			&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
			&expr.Lookup{SourceRegister: 1, DestRegister: 1, IsDestRegSet: true, SetName: lb.eps.Name, SetID: lb.eps.ID},
			&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV6, RegAddrMin: 1, RegProtoMin: 2},
		),
	})
	// rule 2: ip6 daddr <cidr> meta mark 0 queue num 0
	lb.nft.AddRule(&nftables.Rule{
		Table: lb.table, Chain: lb.chain,
		Exprs: append(lb.matchCIDR(),
			&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0, 0, 0, 0}},
			&expr.Queue{Num: nfQueueNum},
		),
	})
	return lb.nft.Flush()
}

func (lb *svcLB) matchCIDR() []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 24, Len: 16},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 16, Mask: lb.cidr.Mask, Xor: make([]byte, 16)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: lb.cidr.IP.To16()},
	}
}

func markKey(id uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, id) // native order to match `meta mark`
	return b
}

// idFor returns the mark id for an endpoint, allocating one and adding the
// eps map element on first use. Caller holds lb.mu.
func (lb *svcLB) idFor(ep endpoint) (uint32, error) {
	if id, ok := lb.epID[ep]; ok {
		return id, nil
	}
	id := lb.nextID
	lb.nextID++
	// map value = addr(16) + port(2, big-endian) + pad(2) to a 4-byte boundary
	val := append(net.ParseIP(ep.addr).To16(), byte(ep.port>>8), byte(ep.port), 0, 0)
	if err := lb.nft.SetAddElements(lb.eps, []nftables.SetElement{{Key: markKey(id), Val: val}}); err != nil {
		return 0, err
	}
	if err := lb.nft.Flush(); err != nil {
		return 0, err
	}
	lb.epID[ep] = id
	return id, nil
}

// pick resolves the service (cached) and returns the mark for the chosen
// backend, round-robin. Returns 0 if there are no endpoints.
func (lb *svcLB) pick(vip string, port int, proto string) (uint32, error) {
	key := fmt.Sprintf("%s:%d:%s", vip, port, proto)
	lb.mu.Lock()
	defer lb.mu.Unlock()

	ent := lb.cache[key]
	if ent == nil || time.Now().After(ent.expiry) {
		ans, err := lb.queryFn(svcQuery{VIP: vip, Port: port, Proto: proto})
		if err != nil {
			return 0, err
		}
		if ans.Error != "" {
			return 0, fmt.Errorf("%s", ans.Error)
		}
		newEps := make([]endpoint, len(ans.Endpoints))
		for i, a := range ans.Endpoints {
			newEps[i] = endpoint{addr: a, port: ans.TargetPort}
		}
		lb.reconcileEndpoints(ent, newEps)
		rr := 0
		if ent != nil {
			rr = ent.rr
		}
		ttl := time.Duration(ans.TTLSeconds) * time.Second
		if ttl <= 0 {
			ttl = cacheTTL
		}
		ent = &svcCacheEntry{endpoints: newEps, expiry: time.Now().Add(ttl), rr: rr}
		lb.cache[key] = ent
	}
	if len(ent.endpoints) == 0 {
		return 0, nil
	}
	ep := ent.endpoints[ent.rr%len(ent.endpoints)]
	ent.rr++
	return lb.idFor(ep)
}

// reconcileEndpoints flushes conntrack entries for endpoints that were in the
// previous set but are gone now, so established flows pinned to a removed
// backend reset. Caller holds lb.mu.
func (lb *svcLB) reconcileEndpoints(old *svcCacheEntry, newEps []endpoint) {
	if old == nil {
		return
	}
	nowSet := map[endpoint]bool{}
	for _, e := range newEps {
		nowSet[e] = true
	}
	for _, e := range old.endpoints {
		if !nowSet[e] {
			flushConntrackTo(e.addr)
			// Keep the eps element/id: harmless (never selected again) and
			// reusing an id before its conntrack drains could misroute an
			// in-flight mark. A real impl would GC ids after a drain check.
		}
	}
}

// flushConntrackTo deletes conntrack entries whose reply source is addr
// (i.e. flows currently DNAT'd to that removed backend).
func flushConntrackTo(addr string) {
	// execd ships no conntrack lib; shell out is fine for the rare removal
	// path. Best-effort.
	_ = exec.Command("conntrack", "-D", "--reply-src", addr).Run()
}

func (lb *svcLB) serve(nf *nfqueue.Nfqueue) error {
	fn := func(a nfqueue.Attribute) int {
		if a.PacketID == nil || a.Payload == nil {
			return 0
		}
		id := *a.PacketID
		vip, port, proto, ok := parseFlow(*a.Payload)
		if !ok {
			nf.SetVerdict(id, nfqueue.NfDrop)
			return 0
		}
		mark, err := lb.pick(vip, port, proto)
		if err != nil || mark == 0 {
			if err != nil {
				log.Printf("svc %s:%d/%s: %v; dropping", vip, port, proto, err)
			} else {
				log.Printf("svc %s:%d/%s: no endpoints; dropping", vip, port, proto)
			}
			nf.SetVerdict(id, nfqueue.NfDrop)
			return 0
		}
		// Set the mark and re-traverse: the packet now hits the DNAT rule.
		if err := nf.SetVerdictWithMark(id, nfqueue.NfRepeat, int(mark)); err != nil {
			log.Printf("svc %s:%d/%s: verdict: %v", vip, port, proto, err)
		}
		return 0
	}
	errFn := func(e error) int {
		log.Printf("nfqueue error: %v", e)
		return 0
	}
	if err := nf.RegisterWithErrorFunc(context.Background(), fn, errFn); err != nil {
		return fmt.Errorf("registering nfqueue handler: %w", err)
	}
	log.Printf("service LB active (mark-based): queuing new flows to %s", lb.cidr)
	select {}
}

// parseFlow extracts (dst ip6, dst port, proto) from an IPv6 packet with a
// TCP or UDP header directly after the fixed header.
func parseFlow(pkt []byte) (vip string, port int, proto string, ok bool) {
	if len(pkt) < 44 || pkt[0]>>4 != 6 {
		return "", 0, "", false
	}
	switch pkt[6] {
	case unix.IPPROTO_TCP:
		proto = "tcp"
	case unix.IPPROTO_UDP:
		proto = "udp"
	default:
		return "", 0, "", false
	}
	dst := net.IP(pkt[24:40])
	port = int(binary.BigEndian.Uint16(pkt[42:44]))
	return dst.String(), port, proto, true
}

func querySvcOverVsock(q svcQuery) (svcAnswer, error) {
	conn, err := vsock.Dial(vsock.Host, svcVsockPort, nil)
	if err != nil {
		return svcAnswer{}, fmt.Errorf("dial host svc port: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(q); err != nil {
		return svcAnswer{}, err
	}
	if _, err := conn.Write([]byte("\n")); err != nil {
		return svcAnswer{}, err
	}
	var ans svcAnswer
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		return svcAnswer{}, fmt.Errorf("no answer from host")
	}
	if err := json.Unmarshal(sc.Bytes(), &ans); err != nil {
		return svcAnswer{}, err
	}
	return ans, nil
}
