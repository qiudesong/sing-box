//go:build with_prometheus

package observability

import C "github.com/sagernet/sing-box/constant"

func connectionLabelNames() []string {
	return []string{C.MetricLabelScope, C.MetricLabelKind, C.MetricLabelTag, C.MetricLabelNetwork}
}

func trafficLabelNames() []string {
	return append(connectionLabelNames(), C.MetricLabelDirection)
}

func dnsLabelNames() []string {
	return []string{C.MetricLabelKind, C.MetricLabelServer, C.MetricLabelQueryType, C.MetricLabelResult}
}

func cacheLabelNames() []string {
	return []string{C.MetricLabelServer, C.MetricLabelResult}
}

func dialLabelNames() []string {
	return []string{C.MetricLabelNetwork, C.MetricLabelOutbound, C.MetricLabelResult}
}

func dialLabelValues(network, outbound string, err error) []string {
	return []string{network, outbound, resultLabel(err)}
}
