package netstack

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/miekg/dns"
)

type cache struct {
	m sync.Map
}

type record struct {
	a       net.IP
	cname   string
	expired time.Time
}

func (c *cache) Load(name string) (net.IP, bool) {
	v, ok := c.m.Load(name)
	if !ok {
		return nil, false
	}
	r := v.(record)
	if time.Now().After(r.expired) {
		return nil, false
	}
	if r.a == nil {
		return c.Load(r.cname)
	}
	return r.a, true
}

func (c *cache) StoreA(name string, value net.IP, ttl uint32) {
	c.m.Store(name, record{a: value, expired: time.Now().Add(time.Second * time.Duration(ttl))})
}

func (c *cache) StoreCNAME(name string, value string, ttl uint32) {
	c.m.Store(name, record{cname: value, expired: time.Now().Add(time.Second * time.Duration(ttl))})
}

type resolver struct {
	cache cache

	conn *dns.Conn

	notifies []chan struct{}
	mu       sync.Mutex
}

func newResolver(network, server string, dial func(context.Context, string, string) (net.Conn, error)) (*resolver, error) {
	conn, err := dial(context.Background(), network, server)
	if err != nil {
		return nil, err
	}
	dnsConn := &dns.Conn{Conn: conn}
	resolver := &resolver{conn: dnsConn}
	go func() {
		for {
			msg, err := resolver.conn.ReadMsg()
			if err != nil {
				log.Println(err)
				continue
			}
			for _, rr := range msg.Answer {
				switch r := rr.(type) {
				case *dns.CNAME:
					resolver.cache.StoreCNAME(r.Header().Name, r.Target, r.Header().Ttl)
				case *dns.A:
					resolver.cache.StoreA(r.Header().Name, r.A, r.Header().Ttl)
				}
			}
			resolver.broadcast()
		}
	}()
	return resolver, nil
}

func (r *resolver) broadcast() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, notify := range r.notifies {
		select {
		case notify <- struct{}{}:
		default:
		}
	}
}

func (r *resolver) addNotify(ch chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifies = append(r.notifies, ch)
}

func (r *resolver) removeNotify(ch chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, ch0 := range r.notifies {
		if ch0 == ch {
			r.notifies = append(r.notifies[:i], r.notifies[i+1:]...)
			return
		}
	}
}

func (r *resolver) lookupIPv4(ctx context.Context, host string) (net.IP, error) {
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.To4() != nil {
			return ip.To4(), nil
		} else {
			return nil, fmt.Errorf("IPv6 address %v is not supported", host)
		}
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	notify := make(chan struct{})
	r.addNotify(notify)
	defer r.removeNotify(notify)
	retried := 0
	for {
		ip, ok := r.cache.Load(dns.Fqdn(host))
		if ok {
			return ip, nil
		}
		if retried > 5 {
			return nil, fmt.Errorf("can't resolve host %v", host)
		}

		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(host), dns.TypeA)
		r.conn.WriteMsg(m)
		select {
		case <-ticker.C:
			retried++
		case <-notify:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (r *resolver) resolveAddrPort(ctx context.Context, address string) (net.IP, uint16, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, 0, err
	}

	ip, err := r.lookupIPv4(ctx, host)
	if err != nil {
		return nil, 0, err
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil, 0, err
	}
	return ip, uint16(p), nil
}
