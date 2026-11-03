package bgpbypass_test

import (
	"context"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/schema"
	"github.com/sagernet/sing-box/service/bgpbypass"
	"github.com/sagernet/sing/common/json"

	"github.com/stretchr/testify/require"
)

func configContext() context.Context {
	out := include.OutboundRegistry()
	svc := include.ServiceRegistry()
	bgpbypass.RegisterService(svc)
	return box.Context(context.Background(), include.InboundRegistry(), out, include.EndpointRegistry(), include.DNSTransportRegistry(), svc, include.CertificateProviderRegistry())
}

func example(t *testing.T) string {
	t.Helper()
	raw, e := os.ReadFile("../../docs/examples/bgp-bypass.json")
	require.NoError(t, e)
	return string(raw)
}

func checkConfig(raw string) error {
	ctx := configContext()
	var o option.Options
	if err := json.UnmarshalContext(ctx, []byte(raw), &o); err != nil {
		return err
	}
	if runtime.GOOS != "linux" && o.Route != nil {
		o.Route.DefaultMark = 0
	}
	instance, err := box.New(box.Options{Context: ctx, Options: o})
	if err == nil {
		err = instance.Close()
	}
	return err
}

func TestExampleConfiguration(t *testing.T) { require.NoError(t, checkConfig(example(t))) }

func TestDefaultOutboundConfiguration(t *testing.T) {
	raw := example(t)
	t.Run("implicit direct", func(t *testing.T) {
		var config map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &config))
		delete(config, "outbounds")
		delete(config, "dns")
		config["route"] = map[string]any{
			"rules": []any{
				map[string]any{"domain": "example.org", "action": "route"},
			},
		}
		content, err := json.Marshal(config)
		require.NoError(t, err)
		require.NoError(t, checkConfig(string(content)))
		delete(config, "route")
		content, err = json.Marshal(config)
		require.NoError(t, err)
		require.NoError(t, checkConfig(string(content)))
	})
	t.Run("explicit proxy", func(t *testing.T) {
		content := strings.Replace(raw, `"final": "direct"`, `"final": "proxy-default"`, 1)
		require.NoError(t, checkConfig(content))
	})
	t.Run("first configured outbound", func(t *testing.T) {
		content := strings.Replace(raw, `"final": "direct",`, "", 1)
		require.NoError(t, checkConfig(content))
	})
	t.Run("unknown final", func(t *testing.T) {
		content := strings.Replace(raw, `"final": "direct"`, `"final": "missing"`, 1)
		require.ErrorContains(t, checkConfig(content), `unknown route outbound "missing"`)
	})
}

func TestConfigurationRejectsUnsafeCombinations(t *testing.T) {
	raw := example(t)
	cases := map[string]string{
		"removed outbound health checks": strings.Replace(raw, `"advertise": false`, `"advertise": false, "health_checks": [{"outbound":"proxy-default","url":"https://example.com"}]`, 1),
		"removed service final":          strings.Replace(raw, `"advertise": false`, `"advertise": false, "final": {"action":"direct"}`, 1),
		"missing learned binding":        strings.Replace(raw, `"bgp_bypass": "bypass-control"`, `"domain_suffix": ["unrelated.test"]`, 1),
		"conditional learned binding":    strings.Replace(raw, `"bgp_bypass": "bypass-control"`, `"bgp_bypass": "bypass-control", "port":443`, 1),
		"inverted learned binding":       strings.Replace(raw, `"bgp_bypass": "bypass-control"`, `"bgp_bypass": "bypass-control", "invert":true`, 1),
		"removed policies":               strings.Replace(raw, `"advertise": false`, `"advertise": false, "policies": [{"domain_suffix":["example.com"],"action":"proxy","outbound":"proxy-default"}]`, 1),
		"unknown service":                strings.Replace(raw, `"bgp_bypass": "bypass-control"`, `"bgp_bypass": "missing"`, 1),
		"unknown field":                  strings.Replace(raw, `"advertise": false`, `"advertise": false, "unknown_field": true`, 1),
		"unknown inbound":                strings.Replace(raw, `"dns_inbounds": [`, `"dns_inbounds": ["missing",`, 1),
		"unknown outbound":               strings.Replace(raw, `"outbound": "proxy-jp"`, `"outbound": "missing"`, 1),
		"invalid AF":                     strings.ReplaceAll(raw, `"ipv6-unicast"`, `"l2vpn-evpn"`),
		"non-expiring DNS":               strings.Replace(raw, `"dns": {`, `"dns": {"disable_expire": true,`, 1),
		"unsafe metrics":                 strings.Replace(raw, `"listen": "127.0.0.1"`, `"listen": "0.0.0.0"`, 1),
		"unsupported rule-set": strings.Replace(raw, `"tag": "proxy-domains",
        "rules": [
          {`, `"tag": "proxy-domains",
        "rules": [
          {"port": [443],`, 1),
		"direct proxy":     strings.ReplaceAll(raw, `"outbound": "proxy-jp"`, `"outbound": "direct"`),
		"dependency cycle": strings.Replace(raw, `"server": "192.0.2.10"`, `"detour": "proxy-default", "server": "192.0.2.10"`, 1),
		"default route":    strings.Replace(raw, `"persistent_routes": []`, `"persistent_routes": [{"ip_cidr":["0.0.0.0/0"],"outbound":"proxy-default"}]`, 1),
		"excluded route":   strings.Replace(raw, `"persistent_routes": []`, `"persistent_routes": [{"ip_cidr":["192.168.66.0/24"],"outbound":"proxy-default"}]`, 1),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) { require.Error(t, checkConfig(content)) })
	}
}

func TestBGPSchema(t *testing.T) {
	raw, err := schema.Generate(configContext(), reflect.TypeFor[option.Options]())
	require.NoError(t, err)
	require.Contains(t, string(raw), `bgp-bypass`)
	require.Contains(t, string(raw), `max_dns_associations`)
	require.NotContains(t, string(raw), `health_checks`)
	require.Contains(t, string(raw), `health_port`)
}
