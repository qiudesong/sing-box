package rule

import (
	"testing"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"

	"github.com/stretchr/testify/require"
)

func TestBypassEmptyRuleSetMetadata(t *testing.T) {
	metadata := buildRuleSetMetadata(nil)
	require.False(t, metadata.ContainsNonDNSBypassRule)
	require.True(t, metadata.IPCIDRUnion)
	require.NotEqual(t, adapter.RuleSetMetadata{}, metadata, "loaded empty sets must differ from unloaded sets")
}

func TestBypassRuleSetSemanticValidation(t *testing.T) {
	ip := option.HeadlessRule{Type: C.RuleTypeDefault, DefaultOptions: option.DefaultHeadlessRule{IPCIDR: []string{"8.8.8.0/24"}}}
	domain := option.HeadlessRule{Type: C.RuleTypeDefault, DefaultOptions: option.DefaultHeadlessRule{DomainSuffix: []string{"example.org"}}}
	require.True(t, buildRuleSetMetadata([]option.HeadlessRule{ip}).IPCIDRUnion)
	logical := option.HeadlessRule{Type: C.RuleTypeLogical, LogicalOptions: option.LogicalHeadlessRule{Mode: C.LogicalTypeAnd, Rules: []option.HeadlessRule{ip, domain}}}
	m := buildRuleSetMetadata([]option.HeadlessRule{logical})
	require.False(t, m.ContainsNonDNSBypassRule)
	require.False(t, m.IPCIDRUnion)
	ip.DefaultOptions.Invert = true
	m = buildRuleSetMetadata([]option.HeadlessRule{ip})
	require.False(t, m.ContainsNonDNSBypassRule)
	require.False(t, m.IPCIDRUnion)
	domain.DefaultOptions.Port = []uint16{443}
	require.True(t, buildRuleSetMetadata([]option.HeadlessRule{domain}).ContainsNonDNSBypassRule)
}
