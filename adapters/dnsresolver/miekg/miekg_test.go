package miekg_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/pericles-luz/crm/adapters/dnsresolver/miekg"
	"github.com/pericles-luz/crm/internal/iam/dnsresolver"
)

// startFakeServer spins up an in-process miekg/dns server on localhost UDP.
// The handler is run on every query so individual tests can install their
// own behaviour by closing over a switch.
func startFakeServer(t *testing.T, handler dns.HandlerFunc) (addr string, stop func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := dns.NewServeMux()
	mux.HandleFunc(".", handler)
	srv := &dns.Server{
		PacketConn: pc,
		Net:        "udp",
		Handler:    mux,
	}
	ready := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(ready) }
	go func() {
		if err := srv.ActivateAndServe(); err != nil {
			// Server.Shutdown returns nil; surface anything else only if
			// it isn't the expected close error.
			if !strings.Contains(err.Error(), "use of closed network connection") {
				t.Logf("server shutdown error: %v", err)
			}
		}
	}()
	<-ready
	return pc.LocalAddr().String(), func() { _ = srv.Shutdown() }
}

func TestLookupIP_AAndAAAAMerged(t *testing.T) {
	addr, stop := startFakeServer(t, func(w dns.ResponseWriter, m *dns.Msg) {
		ans := new(dns.Msg)
		ans.SetReply(m)
		ans.AuthenticatedData = true
		switch m.Question[0].Qtype {
		case dns.TypeA:
			ans.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("203.0.113.10"),
			}}
		case dns.TypeAAAA:
			ans.Answer = []dns.RR{&dns.AAAA{
				Hdr:  dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60},
				AAAA: net.ParseIP("2001:db8::1"),
			}}
		}
		_ = w.WriteMsg(ans)
	})
	defer stop()

	r := miekg.NewResolver(miekg.Config{Server: addr, EnableDNSSEC: true})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := r.LookupIP(ctx, "example.test")
	if err != nil {
		t.Fatalf("LookupIP: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 answers (A+AAAA), got %d: %v", len(got), got)
	}
	wantV4 := netip.MustParseAddr("203.0.113.10")
	wantV6 := netip.MustParseAddr("2001:db8::1")
	seenV4, seenV6 := false, false
	for _, a := range got {
		if a.IP == wantV4 {
			seenV4 = true
			if !a.VerifiedWithDNSSEC {
				t.Errorf("AD bit must propagate as VerifiedWithDNSSEC for %v", a.IP)
			}
		}
		if a.IP == wantV6 {
			seenV6 = true
		}
	}
	if !seenV4 || !seenV6 {
		t.Fatalf("missing IPv4 or IPv6 answer; got %+v", got)
	}
}

func TestLookupIP_NXDOMAIN(t *testing.T) {
	addr, stop := startFakeServer(t, func(w dns.ResponseWriter, m *dns.Msg) {
		ans := new(dns.Msg)
		ans.SetReply(m)
		ans.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(ans)
	})
	defer stop()

	r := miekg.NewResolver(miekg.Config{Server: addr})
	_, err := r.LookupIP(context.Background(), "nonexistent.test")
	if !errors.Is(err, dnsresolver.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLookupIP_NoRecord(t *testing.T) {
	addr, stop := startFakeServer(t, func(w dns.ResponseWriter, m *dns.Msg) {
		ans := new(dns.Msg)
		ans.SetReply(m) // empty Answer + RcodeSuccess
		_ = w.WriteMsg(ans)
	})
	defer stop()

	r := miekg.NewResolver(miekg.Config{Server: addr})
	_, err := r.LookupIP(context.Background(), "empty.test")
	if !errors.Is(err, dnsresolver.ErrNoRecord) {
		t.Fatalf("err = %v, want ErrNoRecord", err)
	}
}

func TestLookupTXT_RFC1035Concatenation(t *testing.T) {
	addr, stop := startFakeServer(t, func(w dns.ResponseWriter, m *dns.Msg) {
		ans := new(dns.Msg)
		ans.SetReply(m)
		ans.Answer = []dns.RR{&dns.TXT{
			Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
			Txt: []string{"crm-verify=", "token-abc"}, // multi-string TXT
		}}
		_ = w.WriteMsg(ans)
	})
	defer stop()

	r := miekg.NewResolver(miekg.Config{Server: addr})
	got, err := r.LookupTXT(context.Background(), "_crm-verify.acme.test")
	if err != nil {
		t.Fatalf("LookupTXT: %v", err)
	}
	if len(got) != 1 || got[0] != "crm-verify=token-abc" {
		t.Fatalf("want concatenated single string, got %v", got)
	}
}

func TestLookupTXT_NXDOMAIN(t *testing.T) {
	addr, stop := startFakeServer(t, func(w dns.ResponseWriter, m *dns.Msg) {
		ans := new(dns.Msg)
		ans.SetReply(m)
		ans.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(ans)
	})
	defer stop()

	r := miekg.NewResolver(miekg.Config{Server: addr})
	_, err := r.LookupTXT(context.Background(), "_crm-verify.nope.test")
	if !errors.Is(err, dnsresolver.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLookupIP_Timeout(t *testing.T) {
	// Server intentionally never replies. The adapter must surface a timeout.
	addr, stop := startFakeServer(t, func(w dns.ResponseWriter, m *dns.Msg) {
		// drop
	})
	defer stop()

	r := miekg.NewResolver(miekg.Config{Server: addr, DialTimeout: 100 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err := r.LookupIP(ctx, "stuck.test")
	if !errors.Is(err, dnsresolver.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestNewResolver_Defaults(t *testing.T) {
	r := miekg.NewResolver(miekg.Config{})
	if r == nil {
		t.Fatal("nil resolver")
	}
}
