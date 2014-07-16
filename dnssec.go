// Copyright (c) 2013 Erik St. Martin, Brian Ketelsen. All rights reserved.
// Use of this source code is governed by The MIT License (MIT) that can be
// found in the LICENSE file.

package main

import (
	"os"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/skynetservices/skydns/cache"
)

var inflight = new(single)

// ParseKeyFile read a DNSSEC keyfile as generated by dnssec-keygen or other
// utilities. It add ".key" for the public key and ".private" for the private key.
func ParseKeyFile(file string) (*dns.DNSKEY, dns.PrivateKey, error) {
	f, e := os.Open(file + ".key")
	if e != nil {
		return nil, nil, e
	}
	k, e := dns.ReadRR(f, file+".key")
	if e != nil {
		return nil, nil, e
	}
	f, e = os.Open(file + ".private")
	if e != nil {
		return nil, nil, e
	}
	p, e := k.(*dns.DNSKEY).ReadPrivateKey(f, file+".private")
	if e != nil {
		return nil, nil, e
	}
	return k.(*dns.DNSKEY), p, nil
}

// sign signs a message m, it takes care of negative or nodata responses as
// well by synthesising NSEC3 records. It will also cache the signatures, using
// a hash of the signed data as a key.
// We also fake the origin TTL in the signature, because we don't want to
// throw away signatures when services decide to have longer TTL. So we just
// set the origTTL to 60.
// TODO(miek): revisit origTTL
func (s *server) sign(m *dns.Msg, bufsize uint16) {
	now := time.Now().UTC()
	incep := uint32(now.Add(-3 * time.Hour).Unix())     // 2+1 hours, be sure to catch daylight saving time and such
	expir := uint32(now.Add(7 * 24 * time.Hour).Unix()) // sign for a week

	for _, r := range rrSets(m.Answer) {
		if r[0].Header().Rrtype == dns.TypeRRSIG {
			continue
		}
		if !dns.IsSubDomain(s.config.Domain, r[0].Header().Name) {
			continue
		}
		if sig, err := s.signSet(r, now, incep, expir); err == nil {
			m.Answer = append(m.Answer, sig)
		}
	}
	for _, r := range rrSets(m.Ns) {
		if r[0].Header().Rrtype == dns.TypeRRSIG {
			continue
		}
		if !dns.IsSubDomain(s.config.Domain, r[0].Header().Name) {
			continue
		}
		if sig, err := s.signSet(r, now, incep, expir); err == nil {
			m.Ns = append(m.Ns, sig)
		}
	}
	for _, r := range rrSets(m.Extra) {
		if r[0].Header().Rrtype == dns.TypeRRSIG || r[0].Header().Rrtype == dns.TypeOPT {
			continue
		}
		if !dns.IsSubDomain(s.config.Domain, r[0].Header().Name) {
			continue
		}
		if sig, err := s.signSet(r, now, incep, expir); err == nil {
			m.Extra = append(m.Extra, sig)
		}
	}
	if bufsize >= 512 || bufsize <= 4096 {
		m.Truncated = m.Len() > int(bufsize)
	}
	o := new(dns.OPT)
	o.Hdr.Name = "."
	o.Hdr.Rrtype = dns.TypeOPT
	o.SetDo()
	o.SetUDPSize(4096) // TODO(miek): echo client
	m.Extra = append(m.Extra, o)
	return
}

func (s *server) signSet(r []dns.RR, now time.Time, incep, expir uint32) (*dns.RRSIG, error) {
	key := cache.Key(r)
	if _, sig, _, exp := s.scache.Search(key); sig != nil { // There can only be one sig in this cache.
		// Is it still valid 24 hours from now?
		if now.Add(+24*time.Hour).Sub(exp) < -24*time.Hour {
			return sig[0].(*dns.RRSIG), nil
		}
		s.scache.Remove(key)
	}
	s.config.log.Infof("scache miss for %s type %d", r[0].Header().Name, r[0].Header().Rrtype)
	StatsDnssecCacheMiss.Inc(1)
	sig, err, shared := inflight.Do(key, func() (*dns.RRSIG, error) {
		sig1 := s.NewRRSIG(incep, expir)
		sig1.Header().Ttl = r[0].Header().Ttl
		if r[0].Header().Rrtype == dns.TypeTXT {
			sig1.OrigTtl = 0
		}
		e := sig1.Sign(s.config.PrivKey, r)
		if e != nil {
			s.config.log.Errorf("failed to sign: %s", e.Error())
		}
		return sig1, e
	})
	if err != nil {
		return nil, err
	}
	if !shared {
		s.scache.InsertSig(key, sig)
	}
	return dns.Copy(sig).(*dns.RRSIG), nil
}

func (s *server) NewRRSIG(incep, expir uint32) *dns.RRSIG {
	sig := new(dns.RRSIG)
	sig.Hdr.Rrtype = dns.TypeRRSIG
	sig.Hdr.Ttl = s.config.Ttl
	sig.OrigTtl = s.config.Ttl
	sig.Algorithm = s.config.PubKey.Algorithm
	sig.KeyTag = s.config.KeyTag
	sig.Inception = incep
	sig.Expiration = expir
	sig.SignerName = s.config.PubKey.Hdr.Name
	return sig
}

type rrset struct {
	qname string
	qtype uint16
}

func rrSets(rrs []dns.RR) map[rrset][]dns.RR {
	m := make(map[rrset][]dns.RR)
	for _, r := range rrs {
		if s, ok := m[rrset{r.Header().Name, r.Header().Rrtype}]; ok {
			s = append(s, r)
			m[rrset{r.Header().Name, r.Header().Rrtype}] = s
		} else {
			s := make([]dns.RR, 1, 3)
			s[0] = r
			m[rrset{r.Header().Name, r.Header().Rrtype}] = s
		}
	}
	if len(m) > 0 {
		return m
	}
	return nil
}

// Adapted from singleinflight.go from the original Go Code. Copyright 2013 The Go Authors.
type call struct {
	wg   sync.WaitGroup
	val  *dns.RRSIG
	err  error
	dups int
}

type single struct {
	sync.Mutex
	m map[string]*call
}

func (g *single) Do(key string, fn func() (*dns.RRSIG, error)) (*dns.RRSIG, error, bool) {
	g.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		g.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.Lock()
	delete(g.m, key)
	g.Unlock()

	return c.val, c.err, c.dups > 0
}
