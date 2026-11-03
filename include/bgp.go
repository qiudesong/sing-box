//go:build with_bgp && (linux || darwin || windows || freebsd || openbsd || netbsd || dragonfly)

package include

import (
	"github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/service/bgpbypass"
)

func registerBGPBypassService(registry *service.Registry) { bgpbypass.RegisterService(registry) }
