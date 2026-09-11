// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package extensions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	pathrs "github.com/cyphar/filepath-securejoin/pathrs-lite"
	"golang.org/x/sys/unix"
)

// ServiceRootfsMountpoint describes a mount destination required inside an
// extension-service rootfs.
type ServiceRootfsMountpoint struct {
	Destination string
	Directory   bool
	// TypeUnknown preserves existing file or directory destinations without
	// creating absent ones until the runtime can inspect the host source.
	TypeUnknown bool
}

// ImplicitServiceRootfsMountpoints returns the files mounted into every
// extension service by the OCI runtime.
func ImplicitServiceRootfsMountpoints() []ServiceRootfsMountpoint {
	return []ServiceRootfsMountpoint{
		{Destination: "/etc/hosts"},
		{Destination: "/etc/resolv.conf"},
	}
}

// ServiceRootfsMountpointIsRuntimeManaged reports whether runc owns creation
// of the destination and its backing pseudo-filesystem.
func ServiceRootfsMountpointIsRuntimeManaged(destination string) bool {
	cleaned := filepath.Clean(destination)

	for _, path := range []string{"/dev", "/proc", "/sys"} {
		if cleaned == path || strings.HasPrefix(cleaned, path+string(os.PathSeparator)) {
			return true
		}
	}

	return false
}

// EnsureServiceRootfsMountpoints creates or validates mount destinations using
// container-root path semantics. Symlinks are resolved within rootfsPath and
// cannot escape it.
func EnsureServiceRootfsMountpoints(rootfsPath string, mountpoints []ServiceRootfsMountpoint) error {
	rootfs, err := os.Open(rootfsPath)
	if err != nil {
		return fmt.Errorf("error opening extension rootfs: %w", err)
	}

	defer rootfs.Close() //nolint:errcheck

	return ensureServiceRootfsMountpoints(rootfs, mountpoints)
}

// EnsureServiceRootfsMountpointsInRoot prepares a service rootfs below an
// extracted extension root. The service root and its ancestors must be real
// directories: following archive-provided symlinks here would change the root
// used for both mountpoint preparation and container-namespace labeling.
func EnsureServiceRootfsMountpointsInRoot(extensionRoot, servicePath string, mountpoints []ServiceRootfsMountpoint) error {
	if !filepath.IsLocal(servicePath) || filepath.Clean(servicePath) == "." {
		return fmt.Errorf("invalid extension service rootfs path %q", servicePath)
	}

	rootfs, err := os.OpenFile(extensionRoot, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("error opening extension rootfs: %w", err)
	}

	for _, component := range strings.Split(filepath.Clean(servicePath), string(os.PathSeparator)) {
		fd, openErr := unix.Openat(int(rootfs.Fd()), component, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = rootfs.Close()
		if openErr != nil {
			return fmt.Errorf("error opening extension service rootfs %q: %w", servicePath, openErr)
		}

		rootfs = os.NewFile(uintptr(fd), component)
	}
	defer rootfs.Close() //nolint:errcheck

	return ensureServiceRootfsMountpoints(rootfs, mountpoints)
}

func ensureServiceRootfsMountpoints(rootfs *os.File, mountpoints []ServiceRootfsMountpoint) error {
	for _, mountpoint := range mountpoints {
		if err := ensureServiceRootfsMountpoint(rootfs, mountpoint); err != nil {
			return err
		}
	}

	return nil
}

func ensureServiceRootfsMountpoint(rootfs *os.File, mountpoint ServiceRootfsMountpoint) error {
	if !filepath.IsAbs(mountpoint.Destination) {
		return fmt.Errorf("extension rootfs mountpoint %q is not absolute", mountpoint.Destination)
	}

	relativePath := strings.TrimPrefix(filepath.Clean(mountpoint.Destination), string(os.PathSeparator))
	if relativePath == "" || relativePath == "." {
		return fmt.Errorf("extension rootfs mountpoint %q targets the rootfs", mountpoint.Destination)
	}

	handle, err := pathrs.OpenatInRoot(rootfs, relativePath)
	switch {
	case err == nil:
		defer handle.Close() //nolint:errcheck

		return validateServiceRootfsMountpoint(handle, mountpoint)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("error inspecting extension rootfs mountpoint %q: %w", mountpoint.Destination, err)
	}

	if mountpoint.TypeUnknown {
		return nil
	}

	if mountpoint.Directory {
		handle, err = pathrs.MkdirAllHandle(rootfs, relativePath, 0o755)
		if err != nil {
			return fmt.Errorf("error creating extension rootfs directory mountpoint %q: %w", mountpoint.Destination, err)
		}
		defer handle.Close() //nolint:errcheck

		return validateServiceRootfsMountpoint(handle, mountpoint)
	}

	parent, err := pathrs.MkdirAllHandle(rootfs, filepath.Dir(relativePath), 0o755)
	if err != nil {
		return fmt.Errorf("error creating extension rootfs mountpoint parent for %q: %w", mountpoint.Destination, err)
	}
	defer parent.Close() //nolint:errcheck

	fileDescriptor, err := unix.Openat(
		int(parent.Fd()),
		filepath.Base(relativePath),
		unix.O_CLOEXEC|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_WRONLY,
		0o644,
	)
	if err != nil {
		return fmt.Errorf("error creating extension rootfs file mountpoint %q: %w", mountpoint.Destination, err)
	}

	if err = unix.Close(fileDescriptor); err != nil {
		return fmt.Errorf("error closing extension rootfs file mountpoint %q: %w", mountpoint.Destination, err)
	}

	handle, err = pathrs.OpenatInRoot(rootfs, relativePath)
	if err != nil {
		return fmt.Errorf("error reopening extension rootfs file mountpoint %q: %w", mountpoint.Destination, err)
	}
	defer handle.Close() //nolint:errcheck

	return validateServiceRootfsMountpoint(handle, mountpoint)
}

func validateServiceRootfsMountpoint(handle *os.File, mountpoint ServiceRootfsMountpoint) error {
	info, err := handle.Stat()
	if err != nil {
		return fmt.Errorf("error inspecting extension rootfs mountpoint %q: %w", mountpoint.Destination, err)
	}

	switch {
	case mountpoint.TypeUnknown:
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("extension rootfs mountpoint %q is not a regular file or directory", mountpoint.Destination)
		}
	case mountpoint.Directory && !info.IsDir():
		return fmt.Errorf("extension rootfs mountpoint %q is not a directory", mountpoint.Destination)
	case !mountpoint.Directory && !info.Mode().IsRegular():
		return fmt.Errorf("extension rootfs mountpoint %q is not a regular file", mountpoint.Destination)
	}

	return nil
}
