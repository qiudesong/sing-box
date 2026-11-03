package bgpbypass

import (
	"bytes"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"

	"github.com/stretchr/testify/require"
)

type loggingTestRouter struct {
	adapter.Router
	tracker adapter.ConnectionTracker
}

func (r *loggingTestRouter) AppendTracker(tracker adapter.ConnectionTracker) {
	r.tracker = tracker
}

func TestInitializeLogsSkippedRules(t *testing.T) {
	s := testService(t)
	s.config.skipped = []int{0, 2}
	router := &loggingTestRouter{}
	s.router = router
	var output bytes.Buffer
	factory, err := log.New(log.Options{
		Context: s.ctx,
		Options: option.LogOptions{
			Level:        "info",
			DisableColor: true,
		},
		DefaultWriter: &output,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, factory.Close()) })
	s.logger = factory.Logger()
	scope := adapter.NewScope(s.ctx, s.logger)
	t.Cleanup(func() { require.NoError(t, scope.Close()) })

	// Initialization logs are queued before the logger starts in a real Box.
	require.NoError(t, s.Start(adapter.StartStateInitialize, scope))
	require.NoError(t, factory.Start())
	require.Contains(t, output.String(), "DNS preflight skips native route rules with connection-only conditions or actions: [0 2]")
	require.Same(t, s, router.tracker)
}
