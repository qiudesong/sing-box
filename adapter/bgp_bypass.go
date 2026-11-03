package adapter

import (
	"context"
	"net/netip"

	"github.com/sagernet/sing-box/option"

	"github.com/miekg/dns"
)

// DNSResponseObserver sees only the final client-visible response. Observers must
// not mutate messages and must bound their work by the caller's context.
type DNSResponseObserver interface {
	ObserveDNSResponse(context.Context, *dns.Msg, *dns.Msg)
}

// DNSResponseObservable is optional: DNSRouter implementations remain compatible.
type DNSResponseObservable interface {
	RegisterDNSResponseObserver(DNSResponseObserver) (unregister func())
	DNSCacheExpires() bool
}

// BGPBypassMatcher exposes a read-only view of effective learned destinations.
// Native route rules choose the outbound; matching never performs control I/O.
type BGPBypassMatcher interface {
	MatchBGPBypass(destination netip.Addr, outbound string) bool
}

// BGPBypassAcquirer confirms a match and protects it from expiry atomically.
// A successful acquisition returns an idempotent release function, or nil for
// a persistent route that needs no connection reference. It performs no I/O.
type BGPBypassAcquirer interface {
	BGPBypassMatcher
	AcquireBGPBypass(destination netip.Addr, outbound string) (release func(), matched bool)
}

// BGPBypassRule acquires only after a rule has become a final route candidate.
// Ordinary Match and PreMatch calls must remain free of connection references.
type BGPBypassRule interface {
	BGPBypassService() string
	AcquireBGPBypass(metadata *InboundContext) (release func(), matched bool)
}

// ConfigurationValidator checks cross-component invariants without starting I/O.
type ConfigurationValidator interface {
	ValidateConfiguration(option.Options) error
}

type RuleSetUpdateValidator interface {
	ValidateRuleSetUpdate(tag string, metadata RuleSetMetadata) error
}
