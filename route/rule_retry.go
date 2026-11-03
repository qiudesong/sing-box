package route

import (
	"errors"
	"slices"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/sniff"
	R "github.com/sagernet/sing-box/route/rule"
	M "github.com/sagernet/sing/common/metadata"
)

// Sniffing consumes input. Reuse its observations at the same rule position
// when route selection retries, without carrying over route-option overrides
// or making sniffed conditions visible to earlier rules.
type ruleSniffResult struct {
	protocol string
	domain   string
	client   string
	context  any
	names    []string
	err      error
}

func snapshotRuleSniff(metadata *adapter.InboundContext) ruleSniffResult {
	return ruleSniffResult{
		protocol: metadata.Protocol,
		domain:   metadata.Domain,
		client:   metadata.Client,
		context:  metadata.SniffContext,
		names:    metadata.SnifferNames,
		err:      metadata.SniffError,
	}
}

func (r ruleSniffResult) restore(metadata *adapter.InboundContext, action *R.RuleActionSniff) {
	if sniff.Skip(metadata) || metadata.Protocol != "" {
		return
	}
	if slices.Equal(metadata.SnifferNames, action.SnifferNames) && metadata.SniffError != nil && !errors.Is(metadata.SniffError, sniff.ErrNeedMoreData) {
		return
	}
	metadata.Protocol = r.protocol
	metadata.Domain = r.domain
	metadata.Client = r.client
	metadata.SniffContext = r.context
	metadata.SnifferNames = r.names
	metadata.SniffError = r.err
	if r.err == nil && action.OverrideDestination && M.IsDomainName(r.domain) {
		metadata.Destination = M.Socksaddr{
			Fqdn: r.domain,
			Port: metadata.Destination.Port,
		}
	}
}
