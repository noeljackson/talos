// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package services_test

import (
	"os"
	"path/filepath"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/internal/app/machined/pkg/system/services"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	extservices "github.com/siderolabs/talos/pkg/machinery/extensions/services"
)

func TestExtensionSELinuxLabel(t *testing.T) {
	t.Parallel()

	assert.Empty(t, services.ExtensionSELinuxLabel(extservices.Security{}))
	assert.Equal(
		t,
		constants.SelinuxLabelWriteableSysfsSysContainer,
		services.ExtensionSELinuxLabel(extservices.Security{WriteableSysfs: true}),
	)
}

func TestEnsureExtensionRootfsMountpoints(t *testing.T) {
	t.Run("creates implicit and declared mountpoints", func(t *testing.T) {
		rootfsPath := t.TempDir()
		directorySource := t.TempDir()
		fileSource := filepath.Join(t.TempDir(), "tun")

		require.NoError(t, os.WriteFile(fileSource, nil, 0o600))

		require.NoError(t, services.EnsureExtensionRootfsMountpoints(rootfsPath, []specs.Mount{
			{Source: directorySource, Destination: "/etc/ssl/certs", Type: "bind"},
			{Source: fileSource, Destination: "/dev/net/tun", Type: "bind"},
			{Source: filepath.Join(t.TempDir(), "absent"), Destination: "/var/lib/tailscale", Type: "bind"},
		}))

		for _, relativePath := range []string{"etc/hosts", "etc/resolv.conf"} {
			info, err := os.Lstat(filepath.Join(rootfsPath, relativePath))
			require.NoError(t, err)
			assert.True(t, info.Mode().IsRegular())
			assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
			assert.Zero(t, info.Size())
		}

		for _, relativePath := range []string{"etc/ssl/certs", "var/lib/tailscale"} {
			info, err := os.Lstat(filepath.Join(rootfsPath, relativePath))
			require.NoError(t, err)
			assert.True(t, info.IsDir())
			assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
		}

		assert.NoDirExists(t, filepath.Join(rootfsPath, "dev"))
	})

	t.Run("leaves OCI runtime managed mountpoints to runc", func(t *testing.T) {
		rootfsPath := t.TempDir()

		require.NoError(t, services.EnsureExtensionRootfsMountpoints(rootfsPath, []specs.Mount{
			{Source: filepath.Join(t.TempDir(), "tun"), Destination: "/dev/net/tun", Type: "bind"},
			{Source: filepath.Join(t.TempDir(), "proc"), Destination: "/proc/custom", Type: "bind"},
			{Source: filepath.Join(t.TempDir(), "sys"), Destination: "/sys/custom", Type: "bind"},
		}))

		for _, path := range []string{"dev", "proc", "sys"} {
			assert.NoDirExists(t, filepath.Join(rootfsPath, path))
		}
	})

	t.Run("preserves artifact-provided regular files", func(t *testing.T) {
		rootfsPath := t.TempDir()
		hostsPath := filepath.Join(rootfsPath, "etc/hosts")

		require.NoError(t, os.MkdirAll(filepath.Dir(hostsPath), 0o755))
		require.NoError(t, os.WriteFile(hostsPath, []byte("artifact-hosts"), 0o600))

		require.NoError(t, services.EnsureExtensionRootfsMountpoints(rootfsPath, nil))

		contents, err := os.ReadFile(hostsPath)
		require.NoError(t, err)
		assert.Equal(t, []byte("artifact-hosts"), contents)

		info, err := os.Lstat(hostsPath)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})

	t.Run("rejects non-regular mountpoints", func(t *testing.T) {
		rootfsPath := t.TempDir()
		hostsPath := filepath.Join(rootfsPath, "etc/hosts")

		require.NoError(t, os.MkdirAll(hostsPath, 0o755))

		err := services.EnsureExtensionRootfsMountpoints(rootfsPath, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a regular file")
	})

	t.Run("rejects a declared mountpoint with the wrong shape", func(t *testing.T) {
		rootfsPath := t.TempDir()
		directorySource := t.TempDir()
		destinationPath := filepath.Join(rootfsPath, "etc/ssl/certs")

		require.NoError(t, os.MkdirAll(filepath.Dir(destinationPath), 0o755))
		require.NoError(t, os.WriteFile(destinationPath, nil, 0o600))

		err := services.EnsureExtensionRootfsMountpoints(rootfsPath, []specs.Mount{
			{Source: directorySource, Destination: "/etc/ssl/certs", Type: "bind"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a directory")
	})

	t.Run("rejects relative declared destinations", func(t *testing.T) {
		err := services.EnsureExtensionRootfsMountpoints(t.TempDir(), []specs.Mount{
			{Source: t.TempDir(), Destination: "var/lib/extension", Type: "bind"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is not absolute")
	})

	t.Run("follows absolute symlinks with container root semantics", func(t *testing.T) {
		rootfsPath := t.TempDir()
		directorySource := t.TempDir()

		require.NoError(t, os.MkdirAll(filepath.Join(rootfsPath, "var"), 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(rootfsPath, "run"), 0o755))
		require.NoError(t, os.Symlink("/run", filepath.Join(rootfsPath, "var/run")))

		require.NoError(t, services.EnsureExtensionRootfsMountpoints(rootfsPath, []specs.Mount{
			{Source: directorySource, Destination: "/var/run/tailscale", Type: "bind"},
		}))

		assert.DirExists(t, filepath.Join(rootfsPath, "run/tailscale"))
		info, err := os.Lstat(filepath.Join(rootfsPath, "var/run"))
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&os.ModeSymlink)
	})

	t.Run("does not follow absolute symlinks outside the container root", func(t *testing.T) {
		rootfsPath := t.TempDir()
		outsidePath := t.TempDir()
		directorySource := t.TempDir()

		require.NoError(t, os.Symlink(outsidePath, filepath.Join(rootfsPath, "var")))
		err := services.EnsureExtensionRootfsMountpoints(rootfsPath, []specs.Mount{
			{Source: directorySource, Destination: "/var/lib/extension", Type: "bind"},
		})
		require.Error(t, err)

		assert.NoDirExists(t, filepath.Join(outsidePath, "lib"))
	})
}
