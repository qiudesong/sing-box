package bgpbypass

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPeerPasswordPlatformSupport(t *testing.T) {
	o := testConfig(t).options
	o.BGP.Peers[0].Password = "test-password"
	err := normalize(&o)
	switch runtime.GOOS {
	case "linux", "android", "openbsd":
		require.NoError(t, err)
	default:
		require.ErrorContains(t, err, "TCP MD5 password is not supported")
	}
}
