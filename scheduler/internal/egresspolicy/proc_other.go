//go:build !linux

package egresspolicy

import (
	"fmt"
	"net"
	"runtime"
)

func enforcementPlatformSupported() error {
	return fmt.Errorf("%w: proxy enforcement needs the Linux egress controller; this host runs %s", ErrEnforcementUnavailable, runtime.GOOS)
}

func verifyControllerPeer(net.Conn) error {
	return enforcementPlatformSupported()
}

func CurrentNetnsInode() (uint64, error) {
	return 0, enforcementPlatformSupported()
}

func PeerCredentials(net.Conn) (PeerCred, error) {
	return PeerCred{}, enforcementPlatformSupported()
}
