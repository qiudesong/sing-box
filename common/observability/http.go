package observability

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sagernet/sing-box/adapter"
)

func (s *Manager) registerHTTPHandlers() {
	if s.options.Metrics {
		s.mainMux.Handle("/metrics", s.metrics)
	}
	s.addCheck("/livez", s.running.Load)
	s.addCheck("/readyz", s.isReady)
}

func (s *Manager) addServiceHandlers(component any) {
	service, ok := component.(adapter.Service)
	if !ok {
		return
	}
	prefix := "/services/" + url.PathEscape(service.Type()) + "/" + url.PathEscape(service.Tag())
	if health, ok := component.(adapter.HealthChecks); ok {
		for name, check := range health.HealthChecks() {
			s.addCheck(prefix+"/"+name, check)
			if name == "readyz" {
				s.checks = append(s.checks, check)
			}
		}
	}
	diagnostics, ok := component.(adapter.Diagnostics)
	if !ok || !s.options.Diagnostics {
		return
	}
	mux := http.NewServeMux()
	diagnostics.RegisterDiagnostics(mux)
	decodedPrefix := "/services/" + service.Type() + "/" + service.Tag()
	s.mainMux.Handle(prefix+"/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := r.Clone(r.Context())
		request.URL.Path = strings.TrimPrefix(r.URL.Path, decodedPrefix)
		request.URL.RawPath = ""
		mux.ServeHTTP(w, request)
	}))
}

func (s *Manager) addCheck(path string, predicate func() bool) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !predicate() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	s.mainMux.Handle(path, handler)
	s.healthMux.Handle(path, handler)
}

func (s *Manager) Handler(healthOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !healthOnly && s.options.Secret != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.options.Secret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if healthOnly {
			s.healthMux.ServeHTTP(w, r)
			return
		}
		s.mainMux.ServeHTTP(w, r)
	})
}
