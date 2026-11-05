package observability

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"

	"github.com/stretchr/testify/require"
)

type testService struct {
	tag   string
	ready bool
}

func (*testService) Type() string                                   { return C.TypeBGPBypass }
func (s *testService) Tag() string                                  { return s.tag }
func (*testService) Start(adapter.StartStage, *adapter.Scope) error { return nil }
func (s *testService) HealthChecks() map[string]func() bool {
	return map[string]func() bool{"livez": func() bool { return true }, "readyz": func() bool { return s.ready }}
}

func (*testService) RegisterDiagnostics(mux *http.ServeMux) {
	mux.HandleFunc("/routes/{ip}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.PathValue("ip")))
	})
}

func TestHealthAndDiagnostics(t *testing.T) {
	manager, err := New(option.ObservabilityOptions{
		ListenPort:  9098,
		Diagnostics: true,
		Secret:      "token",
	}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	first := &testService{tag: "first", ready: true}
	second := &testService{tag: "second /{x}", ready: false}
	manager.Add(first)
	manager.Add(second)
	manager.running.Store(true)
	manager.ready.Store(true)
	request := func(health bool, method, path, secret string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if secret != "" {
			r.Header.Set("Authorization", "Bearer "+secret)
		}
		w := httptest.NewRecorder()
		manager.Handler(health).ServeHTTP(w, r)
		return w
	}
	require.Equal(t, http.StatusUnauthorized, request(false, http.MethodGet, "/livez", "").Code)
	require.Equal(t, http.StatusOK, request(true, http.MethodGet, "/livez", "").Code)
	require.Equal(t, http.StatusServiceUnavailable, request(true, http.MethodGet, "/readyz", "").Code)
	second.ready = true
	require.Equal(t, http.StatusOK, request(true, http.MethodGet, "/readyz", "").Code)
	prefix := "/services/" + C.TypeBGPBypass + "/" + url.PathEscape(second.tag)
	require.Equal(t, http.StatusOK, request(true, http.MethodGet, prefix+"/readyz", "").Code)
	require.Equal(t, "8.8.8.8", request(false, http.MethodGet, prefix+"/routes/8.8.8.8", "token").Body.String())
	for _, path := range []string{prefix + "/routes/8.8.8.8", "/metrics", "/routes", "/dns-readyz", "/services/first/readyz", "/services/other/first/readyz"} {
		require.Equal(t, http.StatusNotFound, request(true, http.MethodGet, path, "").Code, path)
	}
	require.Equal(t, http.StatusMethodNotAllowed, request(true, http.MethodPost, "/livez", "").Code)
	manager.Stop()
	require.Equal(t, http.StatusServiceUnavailable, request(true, http.MethodGet, "/readyz", "").Code)
}

func TestOptionsValidation(t *testing.T) {
	for _, options := range []option.ObservabilityOptions{
		{Metrics: true},
		{Diagnostics: true},
		{Listen: "invalid"},
		{Listen: "0.0.0.0", ListenPort: 9098},
		{HealthListen: "invalid", HealthPort: 9099},
	} {
		_, err := New(options, log.NewNOPFactory().Logger())
		require.Error(t, err)
	}
	_, err := New(option.ObservabilityOptions{}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
}
