// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package extensions_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/internal/pkg/extensions"
)

func TestEnsureServiceRootfsMountpoints(t *testing.T) {
	t.Run("uses container-root symlink semantics", func(t *testing.T) {
		rootfsPath := t.TempDir()

		require.NoError(t, os.MkdirAll(filepath.Join(rootfsPath, "var"), 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(rootfsPath, "run"), 0o755))
		require.NoError(t, os.Symlink("/run", filepath.Join(rootfsPath, "var/run")))

		mountpoints := extensions.ImplicitServiceRootfsMountpoints()
		mountpoints = append(mountpoints, extensions.ServiceRootfsMountpoint{
			Destination: "/var/run/tailscale",
			Directory:   true,
		})

		require.NoError(t, extensions.EnsureServiceRootfsMountpoints(rootfsPath, mountpoints))
		assert.DirExists(t, filepath.Join(rootfsPath, "run/tailscale"))
		assert.FileExists(t, filepath.Join(rootfsPath, "etc/hosts"))
		assert.FileExists(t, filepath.Join(rootfsPath, "etc/resolv.conf"))

		info, err := os.Lstat(filepath.Join(rootfsPath, "var/run"))
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&os.ModeSymlink)
	})

	t.Run("does not follow symlinks outside the container root", func(t *testing.T) {
		rootfsPath := t.TempDir()
		outsidePath := t.TempDir()

		require.NoError(t, os.Symlink(outsidePath, filepath.Join(rootfsPath, "var")))

		err := extensions.EnsureServiceRootfsMountpoints(rootfsPath, []extensions.ServiceRootfsMountpoint{{
			Destination: "/var/lib/extension",
			Directory:   true,
		}})
		require.Error(t, err)
		assert.NoDirExists(t, filepath.Join(outsidePath, "lib"))
	})
}
