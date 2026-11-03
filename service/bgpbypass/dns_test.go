package bgpbypass

import (
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

func rr(t testing.TB, text string) dns.RR {
	t.Helper()
	r, e := dns.NewRR(text)
	require.NoError(t, e)
	return r
}

func TestDNSAliasesHintsAndGlue(t *testing.T) {
	now := time.Now()
	q := new(dns.Msg)
	q.SetQuestion("www.example.", dns.TypeHTTPS)
	r := new(dns.Msg)
	r.SetReply(q)
	r.Answer = []dns.RR{rr(t, "www.example. 30 IN CNAME cdn.example."), rr(t, "cdn.example. 60 IN HTTPS 1 svc.example. ipv4hint=8.8.8.8 ipv6hint=2606:4700:4700::1111")}
	r.Extra = []dns.RR{rr(t, "svc.example. 90 IN A 8.8.4.4"), rr(t, "unrelated.test. 300 IN A 9.9.9.9")}
	leases := responseAssociations(q, r, now)
	require.Len(t, leases, 3)
	for _, a := range leases {
		require.Equal(t, "www.example", a.Domain)
		require.Equal(t, now.Add(30*time.Second), a.Expires)
		require.NotEqual(t, netip.MustParseAddr("9.9.9.9"), a.Address)
	}
	r.Truncated = true
	require.Empty(t, responseAssociations(q, r, now))
	r.Truncated = false
	r.Rcode = dns.RcodeNameError
	require.Empty(t, responseAssociations(q, r, now))
}

func TestDNSZeroTTLAndMultipleAddresses(t *testing.T) {
	now := time.Now()
	q := new(dns.Msg)
	q.SetQuestion("a.example.", dns.TypeA)
	r := new(dns.Msg)
	r.SetReply(q)
	r.Answer = []dns.RR{rr(t, "a.example. 0 IN A 8.8.8.8"), rr(t, "a.example. 45 IN A 8.8.4.4")}
	values := responseAssociations(q, r, now)
	require.Len(t, values, 2)
	for _, a := range values {
		if a.Address.String() == "8.8.8.8" {
			require.Equal(t, now.Add(time.Second), a.Expires)
		}
	}
	require.EqualValues(t, 0, r.Answer[0].Header().Ttl)
}

func TestAdditionalRequiresAnswerReference(t *testing.T) {
	now := time.Now()
	q := new(dns.Msg)
	q.SetQuestion("example.org.", dns.TypeHTTPS)
	r := new(dns.Msg)
	r.SetReply(q)
	r.Extra = []dns.RR{rr(t, "example.org. 90 IN A 8.8.8.8")}
	require.Empty(t, responseAssociations(q, r, now))
	r.Answer = []dns.RR{rr(t, "example.org. 10 IN HTTPS 1 .")}
	a := responseAssociations(q, r, now)
	require.Len(t, a, 1)
	require.Equal(t, now.Add(10*time.Second), a[0].Expires)
}

func FuzzDNSLearning(f *testing.F) {
	q := new(dns.Msg)
	q.SetQuestion("example.org.", dns.TypeA)
	raw, _ := q.Pack()
	f.Add(raw)
	f.Fuzz(func(t *testing.T, data []byte) {
		var r dns.Msg
		if r.Unpack(data) != nil {
			return
		}
		values := responseAssociations(q, &r, time.Unix(0, 0))
		if len(values) > 256 {
			t.Fatal("unbounded result")
		}
	})
}
