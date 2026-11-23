// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mergedWithBlockfile = `
version = 3

[plugins."io.containerd.snapshotter.v1.blockfile"]
scratch_file = "/var/lib/codewire-blockfile/scratch.ext4"
fs_type = "ext4"

[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc]
runtime_type = "io.containerd.runc.v2"

[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.kata-clh]
runtime_type = "io.containerd.kata-clh.v2"
snapshotter = "blockfile"

[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.kata-qemu-snp]
runtime_type = "io.containerd.kata-qemu-snp.v2"
snapshotter = "blockfile"
`

func TestBlockfileScratch(t *testing.T) {
	t.Parallel()

	scratch, err := blockfileScratch([]byte(mergedWithBlockfile))
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/codewire-blockfile/scratch.ext4", scratch)

	scratch, err = blockfileScratch([]byte("version = 3\n[plugins.\"io.containerd.cri.v1.images\"]\nsnapshotter = \"overlayfs\"\n"))
	require.NoError(t, err)
	assert.Empty(t, scratch, "no blockfile section, nothing to create")

	_, err = blockfileScratch([]byte("[plugins.\"io.containerd.snapshotter.v1.blockfile\"]\nscratch_file = \"/var/lib/x\"\nfs_type = \"xfs\"\n"))
	assert.ErrorContains(t, err, "is not ext4")
}

// The fallback fails closed per workload: every runtime on blockfile goes, so
// its Pods are refused rather than run on another snapshotter.
func TestWithoutBlockfile(t *testing.T) {
	t.Parallel()

	out, dropped, err := withoutBlockfile([]byte(mergedWithBlockfile))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"kata-clh", "kata-qemu-snp"}, dropped)

	var cfg map[string]any

	require.NoError(t, toml.Unmarshal(out, &cfg))

	plugins := cfg["plugins"].(map[string]any)
	assert.NotContains(t, plugins, blockfilePlugin)

	runtimes, ok := lookupTable(cfg, "plugins", criRuntimePlugin, "containerd", "runtimes")
	require.True(t, ok)
	assert.Contains(t, runtimes, "runc")
	assert.NotContains(t, runtimes, "kata-clh")
	assert.NotContains(t, runtimes, "kata-qemu-snp")
}

func TestWriteBlockfileFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	base := "version = 3\ndisabled_plugins = [\"io.containerd.snapshotter.v1.erofs\"]\nimports = [\"/etc/cri/conf.d/cri.toml\"]\n"

	path, dropped, err := writeBlockfileFallback([]byte(base), []byte(mergedWithBlockfile), "", "", dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "containerd.toml"), path)
	assert.Len(t, dropped, 2)

	var cfg map[string]any

	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, toml.Unmarshal(contents, &cfg))
	assert.Equal(t, []any{filepath.Join(dir, "cri.toml")}, cfg["imports"])
	assert.Equal(t, []any{"io.containerd.snapshotter.v1.erofs", blockfilePlugin}, cfg["disabled_plugins"])

	cri, err := os.ReadFile(filepath.Join(dir, "cri.toml"))
	require.NoError(t, err)
	assert.NotContains(t, string(cri), "blockfile")
}

// testCreator works in a temporary root owned by the test's own user, with
// the ext4 tools faked: run records each command and leaves the file as is.
func testCreator(t *testing.T) (scratchCreator, string, *[][]string) {
	t.Helper()

	root := t.TempDir() + "/"

	var commands [][]string

	return scratchCreator{
		root:  root,
		owner: os.Getuid(),
		size:  1 << 20,
		run: func(_ context.Context, name string, args ...string) error {
			commands = append(commands, append([]string{name}, args...))

			return nil
		},
	}, root, &commands
}

func TestScratchCreatorCreatesAMissingImage(t *testing.T) {
	t.Parallel()

	c, root, commands := testCreator(t)
	path := filepath.Join(root, "codewire-blockfile", "scratch.ext4")

	// A creation cut short left a temporary file behind.
	require.NoError(t, os.Mkdir(filepath.Join(root, "codewire-blockfile"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "codewire-blockfile", ".scratch-0123456789abcdef"), nil, 0o600))

	created, err := c.ensure(t.Context(), path)
	require.NoError(t, err)
	assert.True(t, created)

	info, err := os.Lstat(path)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
	assert.Equal(t, int64(1<<20), info.Size())
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	leftovers, err := filepath.Glob(filepath.Join(root, "codewire-blockfile", ".scratch-*"))
	require.NoError(t, err)
	assert.Empty(t, leftovers)

	require.Len(t, *commands, 3)
	assert.Equal(t, []string{"mkfs.ext4", "-q", "-F", "-E", "lazy_itable_init=0,lazy_journal_init=0"}, (*commands)[0][:5])
	assert.Equal(t, []string{"debugfs", "-w", "-R", "ea_set <2> security.selinux " + blockfileScratchLabel}, (*commands)[1][:4])
	assert.Equal(t, []string{"e2fsck", "-fn"}, (*commands)[2][:2])

	// An image that exists is left as it is.
	created, err = c.ensure(t.Context(), path)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Len(t, *commands, 3)
}

func TestScratchCreatorRefuses(t *testing.T) {
	t.Parallel()

	t.Run("outside the root", func(t *testing.T) {
		t.Parallel()

		c, _, _ := testCreator(t)

		_, err := c.ensure(t.Context(), "/etc/scratch.ext4")
		assert.ErrorContains(t, err, "is not a clean path under")
	})

	t.Run("an unclean path", func(t *testing.T) {
		t.Parallel()

		c, root, _ := testCreator(t)

		_, err := c.ensure(t.Context(), root+"a/../../scratch.ext4")
		assert.ErrorContains(t, err, "is not a clean path under")
	})

	t.Run("a symlinked directory", func(t *testing.T) {
		t.Parallel()

		c, root, commands := testCreator(t)
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "codewire-blockfile")))

		_, err := c.ensure(t.Context(), filepath.Join(root, "codewire-blockfile", "scratch.ext4"))
		assert.ErrorContains(t, err, "is not a directory owned by")
		assert.Empty(t, *commands)
	})

	t.Run("a symlinked image", func(t *testing.T) {
		t.Parallel()

		c, root, commands := testCreator(t)
		require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(root, "scratch.ext4")))

		_, err := c.ensure(t.Context(), filepath.Join(root, "scratch.ext4"))
		assert.ErrorContains(t, err, "is not a regular file")
		assert.Empty(t, *commands)
	})

	t.Run("a directory of another owner", func(t *testing.T) {
		t.Parallel()

		c, root, commands := testCreator(t)
		c.owner = os.Getuid() + 1

		_, err := c.ensure(t.Context(), filepath.Join(root, "scratch.ext4"))
		assert.ErrorContains(t, err, "is not a directory owned by")
		assert.Empty(t, *commands)
	})
}
