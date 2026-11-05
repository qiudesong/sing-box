//go:build !with_prometheus

package observability

import (
	"net/http"

	"github.com/sagernet/sing-box/adapter"
	E "github.com/sagernet/sing/common/exceptions"
)

func newMetrics(*Manager) (http.Handler, adapter.MetricsObserver, error) {
	return nil, nil, E.New("Prometheus is not included in this build, rebuild with -tags with_prometheus")
}
