package box_test

import (
	"context"
	"testing"

	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	"github.com/stretchr/testify/require"
)

func TestLegacyBGPObservabilityRejected(t *testing.T) {
	var options option.Options
	err := json.UnmarshalContext(include.Context(context.Background()), []byte(`{"services":[{"type":"bgp-bypass","tag":"test","observability":{"metrics":true}}]}`), &options)
	require.ErrorContains(t, err, "observability")
}
