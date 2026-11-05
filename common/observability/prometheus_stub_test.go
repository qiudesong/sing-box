//go:build !with_prometheus

package observability

import (
	"testing"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"

	"github.com/stretchr/testify/require"
)

func TestMissingPrometheus(t *testing.T) {
	_, err := New(option.ObservabilityOptions{ListenPort: 9098, Metrics: true}, log.NewNOPFactory().Logger())
	require.ErrorContains(t, err, "with_prometheus")
}
