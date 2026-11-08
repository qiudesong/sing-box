package rule

import (
	"context"
	"reflect"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/service"

	"go4.org/netipx"
)

func NewRuleSet(ctx context.Context, logger logger.ContextLogger, tag string, options option.RuleSet) (adapter.RuleSet, error) {
	switch options.Type {
	case C.RuleSetTypeInline, C.RuleSetTypeLocal, "":
		return NewLocalRuleSet(ctx, logger, tag, options)
	case C.RuleSetTypeRemote:
		return NewRemoteRuleSet(ctx, logger, tag, options)
	default:
		return nil, E.New("unknown rule-set type: ", options.Type)
	}
}

func extractIPSetFromRule(rawRule adapter.HeadlessRule) []*netipx.IPSet {
	switch rule := rawRule.(type) {
	case *DefaultHeadlessRule:
		return common.FlatMap(rule.destinationIPCIDRItems, func(rawItem RuleItem) []*netipx.IPSet {
			switch item := rawItem.(type) {
			case *IPCIDRItem:
				return []*netipx.IPSet{item.ipSet}
			default:
				return nil
			}
		})
	case *LogicalHeadlessRule:
		return common.FlatMap(rule.rules, extractIPSetFromRule)
	default:
		panic("unexpected rule type")
	}
}

func HasHeadlessRule(rules []option.HeadlessRule, cond func(rule option.DefaultHeadlessRule) bool) bool {
	for _, rule := range rules {
		switch rule.Type {
		case C.RuleTypeDefault:
			if cond(rule.DefaultOptions) {
				return true
			}
		case C.RuleTypeLogical:
			if HasHeadlessRule(rule.LogicalOptions.Rules, cond) {
				return true
			}
		}
	}
	return false
}

func isProcessHeadlessRule(rule option.DefaultHeadlessRule) bool {
	return len(rule.ProcessName) > 0 || len(rule.ProcessPath) > 0 || len(rule.ProcessPathRegex) > 0 || len(rule.PackageName) > 0 || len(rule.PackageNameRegex) > 0
}

func isWIFIHeadlessRule(rule option.DefaultHeadlessRule) bool {
	return len(rule.WIFISSID) > 0 || len(rule.WIFIBSSID) > 0
}

func isIPCIDRHeadlessRule(rule option.DefaultHeadlessRule) bool {
	return len(rule.IPCIDR) > 0 || rule.IPSet != nil
}

func isDNSQueryTypeHeadlessRule(rule option.DefaultHeadlessRule) bool {
	return len(rule.QueryType) > 0
}

func isNonIPCIDRHeadlessRule(rule option.DefaultHeadlessRule) bool {
	ipOnly := option.DefaultHeadlessRule{
		IPCIDR: rule.IPCIDR,
		IPSet:  rule.IPSet,
		Invert: rule.Invert,
	}
	return !reflect.DeepEqual(rule, ipOnly)
}

func buildRuleSetMetadata(headlessRules []option.HeadlessRule) adapter.RuleSetMetadata {
	return adapter.RuleSetMetadata{
		ContainsNonDNSBypassRule: !bypassRulesSupported(headlessRules, false),
		IPCIDRUnion:              bypassRulesSupported(headlessRules, true),
		ContainsProcessRule:      HasHeadlessRule(headlessRules, isProcessHeadlessRule),
		ContainsWIFIRule:         HasHeadlessRule(headlessRules, isWIFIHeadlessRule),
		ContainsIPCIDRRule:       HasHeadlessRule(headlessRules, isIPCIDRHeadlessRule),
		ContainsDNSQueryTypeRule: HasHeadlessRule(headlessRules, isDNSQueryTypeHeadlessRule),
		ContainsNonIPCIDRRule:    HasHeadlessRule(headlessRules, isNonIPCIDRHeadlessRule),
	}
}

func validateRuleSetMetadataUpdate(ctx context.Context, tag string, metadata adapter.RuleSetMetadata) error {
	if manager := service.FromContext[adapter.ServiceManager](ctx); manager != nil {
		for _, component := range manager.Services() {
			if validator, ok := component.(adapter.RuleSetUpdateValidator); ok {
				if err := validator.ValidateRuleSetUpdate(tag, metadata); err != nil {
					return err
				}
			}
		}
	}
	validator := service.FromContext[adapter.DNSRuleSetUpdateValidator](ctx)
	if validator == nil {
		return nil
	}
	return validator.ValidateRuleSetMetadataUpdate(tag, metadata)
}

// Validate semantics before ExtractIPSet: that API intentionally flattens rules.
func bypassRulesSupported(rules []option.HeadlessRule, prefixesOnly bool) bool {
	// An empty list is a valid empty union. IPCIDRUnion remains true for a
	// loaded empty set, distinguishing it from unloaded zero-value metadata.
	for _, raw := range rules {
		switch raw.Type {
		case C.RuleTypeDefault, "":
			r := raw.DefaultOptions
			allowed := option.DefaultHeadlessRule{IPCIDR: r.IPCIDR, IPSet: r.IPSet}
			if prefixesOnly {
				if len(r.IPCIDR) == 0 && r.IPSet == nil {
					return false
				}
			} else {
				allowed.Domain = r.Domain
				allowed.DomainSuffix = r.DomainSuffix
				allowed.DomainKeyword = r.DomainKeyword
				allowed.DomainRegex = r.DomainRegex
				allowed.DomainMatcher = r.DomainMatcher
				allowed.AdGuardDomain = r.AdGuardDomain
				allowed.AdGuardDomainMatcher = r.AdGuardDomainMatcher
				allowed.Invert = r.Invert
			}
			if !reflect.DeepEqual(r, allowed) {
				return false
			}
		case C.RuleTypeLogical:
			if prefixesOnly && (raw.LogicalOptions.Invert || raw.LogicalOptions.Mode != C.LogicalTypeOr) {
				return false
			}
			if !bypassRulesSupported(raw.LogicalOptions.Rules, prefixesOnly) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
