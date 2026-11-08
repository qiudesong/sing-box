//go:build !with_bgp || !(linux || darwin || windows || freebsd || openbsd || netbsd || dragonfly)

package include

import (
	"context"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/service"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
)

func registerBGPBypassService(registry *service.Registry) {
	service.Register[option.BGPBypassServiceOptions](registry, C.TypeBGPBypass, func(ctx context.Context, logger log.ContextLogger, tag string, options option.BGPBypassServiceOptions) (adapter.Service, error) {
		return nil, E.New("bgp-bypass requires -tags with_bgp on a GoBGP-supported platform (Linux, macOS, Windows, FreeBSD, OpenBSD, NetBSD or DragonFly BSD)")
	})
}
