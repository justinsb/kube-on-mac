package main

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Lazy cluster DNS, same shape as the service LB: there is no cluster-wide
// DNS server to run or keep in sync. execd serves DNS on the pod's loopback;
// names under svc.cluster.local are resolved by asking the host agent over
// the existing vsock service channel (cached briefly), everything else is
// forwarded verbatim to the upstream (gvproxy) resolver. resolv.conf gets
// the standard kubelet search path, so `redis-leader` works from any image.

const dnsCacheTTL = 5 * time.Second

type dnsProxy struct {
	upstream string // e.g. 192.168.127.1:53

	mu    sync.Mutex
	cache map[string]dnsCacheEntry // "name.ns" -> ClusterIPs
}

type dnsCacheEntry struct {
	ips    []net.IP
	found  bool
	expiry time.Time
}

// startDNSProxy binds the loopback DNS sockets; serving runs in goroutines.
// Binding happens before the workload starts so the resolver is never absent.
func startDNSProxy(upstream string) error {
	p := &dnsProxy{upstream: upstream, cache: map[string]dnsCacheEntry{}}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53})
	if err != nil {
		return fmt.Errorf("binding 127.0.0.1:53: %w", err)
	}
	go p.serve(conn)
	if c6, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 53}); err == nil {
		go p.serve(c6)
	}
	return nil
}

func (p *dnsProxy) serve(conn *net.UDPConn) {
	buf := make([]byte, 4096)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		q := make([]byte, n)
		copy(q, buf[:n])
		go func() {
			if resp := p.handle(q); resp != nil {
				conn.WriteToUDP(resp, addr)
			}
		}()
	}
}

func (p *dnsProxy) handle(query []byte) []byte {
	var msg dnsmessage.Message
	if err := msg.Unpack(query); err != nil || len(msg.Questions) != 1 {
		return p.forward(query)
	}
	q := msg.Questions[0]
	name := strings.ToLower(q.Name.String()) // FQDN with trailing dot
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")

	if len(labels) < 3 || strings.Join(labels[len(labels)-3:], ".") != "svc.cluster.local" {
		if strings.HasSuffix(name, ".cluster.local.") {
			return dnsReply(msg, q, nil, dnsmessage.RCodeNameError)
		}
		return p.forward(query)
	}
	// <service>.<namespace>.svc.cluster.local (pod/SRV forms not supported)
	if len(labels) != 5 {
		return dnsReply(msg, q, nil, dnsmessage.RCodeNameError)
	}
	svc, ns := labels[0], labels[1]
	ent := p.lookup(svc, ns)
	if !ent.found {
		return dnsReply(msg, q, nil, dnsmessage.RCodeNameError)
	}
	var ips []net.IP
	for _, ip := range ent.ips {
		if (q.Type == dnsmessage.TypeAAAA) == (ip.To4() == nil) {
			ips = append(ips, ip)
		}
	}
	return dnsReply(msg, q, ips, dnsmessage.RCodeSuccess)
}

// lookup resolves a service name to its ClusterIPs via the host agent,
// with a short cache (the agent answer is authoritative apiserver state).
func (p *dnsProxy) lookup(svc, ns string) dnsCacheEntry {
	key := svc + "." + ns
	p.mu.Lock()
	ent, ok := p.cache[key]
	p.mu.Unlock()
	if ok && time.Now().Before(ent.expiry) {
		return ent
	}
	ent = dnsCacheEntry{expiry: time.Now().Add(dnsCacheTTL)}
	ans, err := querySvcOverVsock(svcQuery{Name: svc, Namespace: ns})
	if err != nil {
		log.Printf("dns: resolving %s: %v", key, err)
		return ent // negative, uncached error path
	}
	if ans.Error == "" {
		ent.found = true
		for _, s := range ans.ClusterIPs {
			if ip := net.ParseIP(s); ip != nil {
				ent.ips = append(ent.ips, ip)
			}
		}
		if ans.TTLSeconds > 0 {
			ent.expiry = time.Now().Add(time.Duration(ans.TTLSeconds) * time.Second)
		}
	}
	p.mu.Lock()
	p.cache[key] = ent
	p.mu.Unlock()
	return ent
}

func dnsReply(req dnsmessage.Message, q dnsmessage.Question, ips []net.IP, rcode dnsmessage.RCode) []byte {
	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:            req.ID,
			Response:      true,
			Authoritative: true,
			RCode:         rcode,
		},
		Questions: []dnsmessage.Question{q},
	}
	for _, ip := range ips {
		hdr := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 5}
		if ip4 := ip.To4(); ip4 != nil && q.Type == dnsmessage.TypeA {
			var a [4]byte
			copy(a[:], ip4)
			hdr.Type = dnsmessage.TypeA
			resp.Answers = append(resp.Answers, dnsmessage.Resource{
				Header: hdr, Body: &dnsmessage.AResource{A: a}})
		} else if ip4 == nil && q.Type == dnsmessage.TypeAAAA {
			var aaaa [16]byte
			copy(aaaa[:], ip.To16())
			hdr.Type = dnsmessage.TypeAAAA
			resp.Answers = append(resp.Answers, dnsmessage.Resource{
				Header: hdr, Body: &dnsmessage.AAAAResource{AAAA: aaaa}})
		}
	}
	out, err := resp.Pack()
	if err != nil {
		return nil
	}
	return out
}

func (p *dnsProxy) forward(query []byte) []byte {
	if p.upstream == "" {
		return nil
	}
	conn, err := net.Dial("udp", p.upstream)
	if err != nil {
		return nil
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(query); err != nil {
		return nil
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}
