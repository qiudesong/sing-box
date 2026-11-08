package ssmapi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMetricsDisabledReturnsOriginalTracker(t *testing.T) {
	traffic := NewTrafficManager()
	require.Same(t, traffic, newMetricsTracker(traffic, nil, "ssm:in"))
}
