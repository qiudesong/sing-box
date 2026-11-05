package observability

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"
)

func normalizeHTTPOptions(options *option.ObservabilityOptions) error {
	if options.Listen == "" {
		options.Listen = "127.0.0.1"
	}
	if options.HealthListen == "" {
		options.HealthListen = "127.0.0.1"
	}
	if (options.Metrics || options.Diagnostics) && options.ListenPort == 0 {
		return E.New("observability metrics and diagnostics require listen_port")
	}
	for _, address := range []string{options.Listen, options.HealthListen} {
		if _, err := netip.ParseAddr(address); err != nil {
			return E.Cause(err, "observability listen address")
		}
	}
	if address := netip.MustParseAddr(options.Listen); options.ListenPort != 0 && !address.IsLoopback() && options.Secret == "" {
		return E.New("non-loopback observability requires secret")
	}
	return nil
}

func (s *Manager) startHTTPServers(scope *adapter.Scope) error {
	for _, listener := range []struct {
		address string
		port    uint16
		health  bool
	}{
		{s.options.Listen, s.options.ListenPort, false},
		{s.options.HealthListen, s.options.HealthPort, true},
	} {
		if listener.port == 0 {
			continue
		}
		if err := s.listen(scope, listener.address, listener.port, listener.health); err != nil {
			return err
		}
	}
	return nil
}

func (s *Manager) listen(scope *adapter.Scope, address string, port uint16, health bool) error {
	listener, err := net.Listen(N.NetworkTCP, net.JoinHostPort(address, strconv.Itoa(int(port))))
	if err != nil {
		return E.Cause(err, "listen observability")
	}
	server := &http.Server{
		Handler:           s.Handler(health),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8192,
	}
	var workers sync.WaitGroup
	workers.Go(func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("serve observability: ", err)
		}
	})
	scope.Add(func() error {
		err := server.Close()
		workers.Wait()
		return err
	})
	return nil
}
