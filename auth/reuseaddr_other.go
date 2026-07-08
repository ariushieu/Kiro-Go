//go:build !windows

package auth

import "syscall"

// setReuseAddr enables SO_REUSEADDR on the raw socket fd. On Unix the fd is an int.
// Go already sets this on Unix listeners by default; setting it explicitly keeps
// the SSO callback rebind behavior identical across platforms.
func setReuseAddr(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
