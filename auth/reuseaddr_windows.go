//go:build windows

package auth

import "syscall"

// soExclusiveAddrUse is SO_EXCLUSIVEADDRUSE. The Go syscall package does not export
// it; Winsock defines it as ((int)(~SO_REUSEADDR)) == -5.
const soExclusiveAddrUse = -5

// setReuseAddr hardens the loopback OAuth-callback listener on Windows.
//
// It deliberately does NOT set SO_REUSEADDR: on Windows SO_REUSEADDR lets a
// DIFFERENT (possibly malicious) local process bind the SAME address already in
// use and race to receive the OAuth callback (L6). Instead it sets
// SO_EXCLUSIVEADDRUSE, Microsoft's documented-correct option, which reserves the
// address for this process and blocks any other socket from stealing it.
//
// The original reason SO_REUSEADDR was set here — avoiding a WSAEADDRINUSE when a
// just-closed port is still in TIME_WAIT on an immediate SSO retry — is covered by
// bindKiroLoopback iterating over a pool of ports: a port stuck in TIME_WAIT is
// simply skipped for the next one.
func setReuseAddr(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, soExclusiveAddrUse, 1)
}
