package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/florianl/go-nfqueue"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

// Lazy ClusterIP load balancing (see research/services.md). At boot we
// install one nftables rule: new connections to the service CIDR are queued
// to userspace (here). On the first packet of a flow to an unseen VIP we ask
// the host agent for endpoints, install a per-VIP DNAT rule ahead of the
// queue rule, and NF_REPEAT the packet so it re-traverses and gets DNAT'd.
// Every later flow to that VIP is handled entirely in-kernel.

const (
	svcVsockPort = 1025
	nfQueueNum   = 0
	svcTable     = "kube"
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

type svcLB struct {
	cidr    *net.IPNet
	nft     *nftables.Conn
	table   *nftables.Table
	chain   *nftables.Chain
	queue   *nftables.Rule // the fallthrough queue rule (VIP rules go above it)
	mu      sync.Mutex
	known   map[string]time.Time // "vip:port:proto" -> expiry
	queryFn func(svcQuery) (svcAnswer, error)
}

// setupServiceLB installs the base nft table and starts the NFQUEUE reader.
func setupServiceLB(cidr string) error {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("parsing service cidr: %w", err)
	}
	lb := &svcLB{
		cidr:    ipnet,
		nft:     &nftables.Conn{},
		known:   map[string]time.Time{},
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
	lb.chain = lb.nft.AddChain(&nftables.Chain{
		Name:     "output",
		Table:    lb.table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityNATDest,
	})
	// ip6 daddr <cidr> ct state new  queue num 0
	lb.queue = lb.nft.AddRule(&nftables.Rule{
		Table: lb.table,
		Chain: lb.chain,
		Exprs: []expr.Any{
			&expr.Payload{ // ip6 daddr (offset 24, len 16)
				DestRegister: 1, Base: expr.PayloadBaseNetworkHeader,
				Offset: 24, Len: 16,
			},
			&expr.Bitwise{
				SourceRegister: 1, DestRegister: 1, Len: 16,
				Mask: lb.cidr.Mask, Xor: make([]byte, 16),
			},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: lb.cidr.IP.To16()},
			&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
			&expr.Bitwise{
				SourceRegister: 1, DestRegister: 1, Len: 4,
				Mask: binaryLE(nfCtStateNew), Xor: []byte{0, 0, 0, 0},
			},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
			&expr.Queue{Num: nfQueueNum},
		},
	})
	return lb.nft.Flush()
}

const nfCtStateNew = 0x08 // IP_CT_NEW bit in ct state bitmask

func binaryLE(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
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
		key := fmt.Sprintf("%s:%d:%s", vip, port, proto)

		lb.mu.Lock()
		exp, seen := lb.known[key]
		fresh := seen && time.Now().Before(exp)
		lb.mu.Unlock()

		if fresh {
			// Rule already present (race: packet queued before rule took
			// effect). Re-traverse.
			nf.SetVerdict(id, nfqueue.NfRepeat)
			return 0
		}

		ans, err := lb.queryFn(svcQuery{VIP: vip, Port: port, Proto: proto})
		if err != nil || ans.Error != "" || len(ans.Endpoints) == 0 {
			log.Printf("svc %s: no endpoints (%v/%s); dropping", key, err, ans.Error)
			// Short negative cache via reject rule would go here; for now drop.
			nf.SetVerdict(id, nfqueue.NfDrop)
			return 0
		}
		if err := lb.installVIP(vip, port, proto, ans); err != nil {
			log.Printf("svc %s: installing rule: %v", key, err)
			nf.SetVerdict(id, nfqueue.NfDrop)
			return 0
		}
		lb.mu.Lock()
		lb.known[key] = time.Now().Add(time.Duration(ans.TTLSeconds) * time.Second)
		lb.mu.Unlock()
		log.Printf("svc %s: installed DNAT -> %v:%d", key, ans.Endpoints, ans.TargetPort)

		// Re-traverse: now hits the DNAT rule we just inserted.
		nf.SetVerdict(id, nfqueue.NfRepeat)
		return 0
	}
	errFn := func(e error) int {
		log.Printf("nfqueue error: %v", e)
		return 0
	}
	if err := nf.RegisterWithErrorFunc(context.Background(), fn, errFn); err != nil {
		return fmt.Errorf("registering nfqueue handler: %w", err)
	}
	log.Printf("service LB active: queuing new flows to %s", lb.cidr)
	select {} // block forever; the nfqueue runs in its own goroutines
}

// installVIP inserts, ahead of the queue rule:
//
//	ip6 daddr VIP <proto> dport PORT dnat to numgen random mod N map { … }
func (lb *svcLB) installVIP(vip string, port int, proto string, ans svcAnswer) error {
	l4proto := byte(unix.IPPROTO_TCP)
	dportOff := 2 // TCP/UDP dest port at offset 2 in the transport header
	if proto == "udp" {
		l4proto = byte(unix.IPPROTO_UDP)
	}

	// Match: ip6 daddr VIP && l4proto && dport PORT
	matchExprs := []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 24, Len: 16},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: net.ParseIP(vip).To16()},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{l4proto}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: uint32(dportOff), Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binary.BigEndian.AppendUint16(nil, uint16(port))},
	}

	// Load-balance target selection. With one endpoint, a plain immediate;
	// with several, numgen random mod N indexes an anonymous verdict map
	// index->address so subsequent flows balance entirely in-kernel.
	var lbExprs []expr.Any
	if len(ans.Endpoints) == 1 {
		lbExprs = []expr.Any{
			&expr.Immediate{Register: 1, Data: net.ParseIP(ans.Endpoints[0]).To16()},
		}
	} else {
		setElems := make([]nftables.SetElement, len(ans.Endpoints))
		for i, ep := range ans.Endpoints {
			setElems[i] = nftables.SetElement{
				Key: binaryLE(uint32(i)),
				Val: net.ParseIP(ep).To16(),
			}
		}
		m := &nftables.Set{
			Table:     lb.table,
			Anonymous: true,
			Constant:  true,
			IsMap:     true,
			KeyType:   nftables.TypeInteger,
			DataType:  nftables.TypeIP6Addr,
		}
		if err := lb.nft.AddSet(m, setElems); err != nil {
			return fmt.Errorf("addset: %w", err)
		}
		lbExprs = []expr.Any{
			&expr.Numgen{Register: 1, Modulus: uint32(len(ans.Endpoints)), Type: unix.NFT_NG_RANDOM},
			// IsDestRegSet marks this as a data-map lookup (numgen index ->
			// endpoint address). Omitting it produces malformed bytecode
			// that the kernel rejects with EINVAL.
			&expr.Lookup{SourceRegister: 1, DestRegister: 1, IsDestRegSet: true, SetName: m.Name, SetID: m.ID},
		}
	}

	// dnat to reg1 : reg2(port)
	natExprs := []expr.Any{
		&expr.Immediate{Register: 2, Data: binary.BigEndian.AppendUint16(nil, uint16(ans.TargetPort))},
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      unix.NFPROTO_IPV6,
			RegAddrMin:  1,
			RegProtoMin: 2,
		},
	}

	exprs := append(append(matchExprs, lbExprs...), natExprs...)
	lb.nft.InsertRule(&nftables.Rule{ // Insert = prepend, above the queue rule
		Table: lb.table,
		Chain: lb.chain,
		Exprs: exprs,
	})
	return lb.nft.Flush()
}

// parseFlow extracts (dst ip6, dst port, proto) from an IPv6 packet with a
// TCP or UDP header directly after the fixed header (no extension headers,
// which our own traffic won't have).
func parseFlow(pkt []byte) (vip string, port int, proto string, ok bool) {
	if len(pkt) < 40 || pkt[0]>>4 != 6 {
		return "", 0, "", false
	}
	nextHdr := pkt[6]
	dst := net.IP(pkt[24:40])
	switch nextHdr {
	case unix.IPPROTO_TCP:
		proto = "tcp"
	case unix.IPPROTO_UDP:
		proto = "udp"
	default:
		return "", 0, "", false
	}
	if len(pkt) < 44 {
		return "", 0, "", false
	}
	port = int(binary.BigEndian.Uint16(pkt[42:44])) // dport at offset 2 of L4
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
