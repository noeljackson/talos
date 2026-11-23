// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"

	"github.com/siderolabs/talos/internal/pkg/selinux"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// containerd's blockfile snapshotter copies its scratch image into its own
// state when the CRI containerd starts. When the image is missing, containerd
// does not skip the snapshotter: every plugin behind it fails to load, images
// included, and the whole CRI is down. machined therefore creates a missing
// image before the CRI containerd starts and, if that fails, starts it without
// blockfile and without the runtimes that use it, so those workloads are
// refused instead of running on another snapshotter.
const (
	blockfilePlugin  = "io.containerd.snapshotter.v1.blockfile"
	criRuntimePlugin = "io.containerd.cri.v1.runtime"
	criImagesPlugin  = "io.containerd.cri.v1.images"

	// BlockfileScratchSize is the size of a scratch image machined creates.
	// Every container root filesystem on the node starts from this size.
	BlockfileScratchSize = 12 << 30

	// blockfileScratchRoot holds every scratch image machined may create.
	blockfileScratchRoot = "/var/lib/"

	// blockfileScratchLabel is containerd's state label, which every layer
	// unpacked on the image inherits from its root directory.
	blockfileScratchLabel = "system_u:object_r:containerd_state_t:s0"
)

// blockfileScratch returns the scratch_file the merged CRI configuration gives
// the blockfile snapshotter, or "" when blockfile is not configured.
func blockfileScratch(config []byte) (string, error) {
	var cfg map[string]any

	if err := toml.Unmarshal(config, &cfg); err != nil {
		return "", fmt.Errorf("error parsing the CRI configuration: %w", err)
	}

	plugin, _ := lookupTable(cfg, "plugins", blockfilePlugin)
	if plugin == nil {
		return "", nil
	}

	if fsType, ok := plugin["fs_type"].(string); ok && fsType != "" && fsType != "ext4" {
		return "", fmt.Errorf("blockfile fs_type %q is not ext4", fsType)
	}

	scratch, _ := plugin["scratch_file"].(string)

	return scratch, nil
}

// withoutBlockfile returns the merged CRI configuration without the blockfile
// snapshotter, without every runtime whose snapshotter is blockfile and
// without the image platforms that unpack on it, and the names of the runtimes
// it dropped.
func withoutBlockfile(config []byte) ([]byte, []string, error) {
	var cfg map[string]any

	if err := toml.Unmarshal(config, &cfg); err != nil {
		return nil, nil, fmt.Errorf("error parsing the CRI configuration: %w", err)
	}

	if plugins, ok := cfg["plugins"].(map[string]any); ok {
		delete(plugins, blockfilePlugin)
	}

	// Without blockfile there is no safe default left: the default runtime or
	// snapshotter would put every Pod on another snapshotter, or none would start.
	if images, _ := lookupTable(cfg, "plugins", criImagesPlugin); images != nil && images["snapshotter"] == "blockfile" {
		return nil, nil, errors.New("the default snapshotter is blockfile")
	}

	var dropped []string

	containerd, _ := lookupTable(cfg, "plugins", criRuntimePlugin, "containerd")

	if runtimes, _ := lookupTable(cfg, "plugins", criRuntimePlugin, "containerd", "runtimes"); runtimes != nil {
		for name, value := range runtimes {
			if runtime, ok := value.(map[string]any); ok && runtime["snapshotter"] == "blockfile" {
				delete(runtimes, name)

				dropped = append(dropped, name)
			}
		}
	}

	if containerd != nil {
		if name, _ := containerd["default_runtime_name"].(string); slices.Contains(dropped, name) {
			return nil, nil, fmt.Errorf("the default runtime %q uses blockfile", name)
		}
	}

	// The images plugin unpacks each runtime's images on its snapshotter.
	if platforms, _ := lookupTable(cfg, "plugins", criImagesPlugin, "runtime_platforms"); platforms != nil {
		for name, value := range platforms {
			if platform, ok := value.(map[string]any); ok && platform["snapshotter"] == "blockfile" {
				delete(platforms, name)
			}
		}
	}

	out, err := toml.Marshal(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("error writing the CRI configuration: %w", err)
	}

	return out, dropped, nil
}

func lookupTable(cfg map[string]any, keys ...string) (map[string]any, bool) {
	table := cfg

	for _, key := range keys {
		next, ok := table[key].(map[string]any)
		if !ok {
			return nil, false
		}

		table = next
	}

	return table, true
}

// scratchCreator creates a missing scratch image. root, owner and run are
// fields so tests can run it unprivileged.
type scratchCreator struct {
	// root is the directory every scratch image must be under.
	root string
	// owner is the UID every directory on the path must belong to.
	owner int
	// size is the image size in bytes.
	size int64
	// run runs a command to completion.
	run func(ctx context.Context, name string, args ...string) error
}

func newScratchCreator() scratchCreator {
	return scratchCreator{
		root:  blockfileScratchRoot,
		owner: 0,
		size:  BlockfileScratchSize,
		run: func(ctx context.Context, name string, args ...string) error {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
			}

			return nil
		},
	}
}

// ensure creates path when it is missing and reports whether it did. It
// refuses a path outside root, a symlink anywhere on it, and a directory on
// it that the owner does not own, since workloads may reach /var/lib.
func (c scratchCreator) ensure(ctx context.Context, path string) (bool, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasPrefix(path, c.root) {
		return false, fmt.Errorf("scratch file %q is not a clean path under %s", path, c.root)
	}

	dir := filepath.Dir(path)

	if err := c.ensureDirs(dir); err != nil {
		return false, err
	}

	info, err := os.Lstat(path)

	switch {
	case err == nil && info.Mode().IsRegular():
		return false, nil
	case err == nil:
		return false, fmt.Errorf("scratch file %q is not a regular file", path)
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}

	return true, c.create(ctx, dir, path)
}

// ensureDirs checks every directory from root down to dir, creating the
// missing ones, and refuses a symlink, a directory the owner does not own and
// one its group or others can write.
func (c scratchCreator) ensureDirs(dir string) error {
	root := filepath.Clean(c.root)

	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%q is not under %s", dir, root)
	}

	parts := []string{""}
	if rel != "." {
		parts = append(parts, strings.Split(rel, string(filepath.Separator))...)
	}

	current := root

	for _, part := range parts {
		current = filepath.Join(current, part)

		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) && part != "" {
			if err = os.Mkdir(current, 0o700); err != nil {
				return err
			}

			info, err = os.Lstat(current)
		}

		if err != nil {
			return err
		}

		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || !ok || int(stat.Uid) != c.owner {
			return fmt.Errorf("%q is not a directory owned by UID %d", current, c.owner)
		}

		// Anyone else who can write here could swap a name for a symlink between
		// these checks and the path-based creation, or plant temporary files.
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%q is writable by its group or others (mode %#o)", current, info.Mode().Perm())
		}
	}

	return nil
}

// create makes the image codewire-blockfile-scratch makes: an eagerly
// initialized ext4 file system whose root carries containerd's state label,
// checked, then renamed into place without replacing anything there.
func (c scratchCreator) create(ctx context.Context, dir, path string) error {
	// A creation cut short (a power loss during mkfs) leaves its temporary
	// file behind; dir is verified to belong to the owner, so only it wrote them.
	leftovers, err := filepath.Glob(filepath.Join(dir, ".scratch-*"))
	if err != nil {
		return err
	}

	for _, leftover := range leftovers {
		if err = os.Remove(leftover); err != nil {
			return err
		}
	}

	suffix := make([]byte, 8)
	if _, err = rand.Read(suffix); err != nil {
		return err
	}

	tmp := filepath.Join(dir, ".scratch-"+hex.EncodeToString(suffix))

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}

	defer os.Remove(tmp) //nolint:errcheck

	err = f.Truncate(c.size)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		return err
	}

	// mke2fs copies the source directory's own label to the image's root, and
	// Talos ships no debugfs to set it afterwards.
	source := tmp + ".root"

	if err = os.Mkdir(source, 0o700); err != nil {
		return err
	}

	defer os.Remove(source) //nolint:errcheck

	if err = selinux.SetLabel(source, blockfileScratchLabel); err != nil {
		return err
	}

	for _, command := range [][]string{
		{"mkfs.ext4", "-q", "-F", "-E", "lazy_itable_init=0,lazy_journal_init=0", "-d", source, tmp},
		{"e2fsck", "-fn", tmp},
	} {
		if err = c.run(ctx, command[0], command[1:]...); err != nil {
			return err
		}
	}

	if err = os.Chmod(tmp, 0o644); err != nil {
		return err
	}

	return unix.Renameat2(unix.AT_FDCWD, tmp, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE)
}

// blockfileFallbackDir holds the configuration the CRI containerd starts with
// when the scratch image cannot be created.
var blockfileFallbackDir = filepath.Join(constants.SystemRunPath, "cri-blockfile-fallback")

// writeBlockfileFallback writes the base containerd configuration importing a
// copy of the merged CRI configuration without blockfile, with blockfile also
// in disabled_plugins, and returns its path and the runtimes it dropped. Both
// files carry the labels of the files they stand in for.
func writeBlockfileFallback(base, merged []byte, baseLabel, mergedLabel, dir string) (string, []string, error) {
	filtered, dropped, err := withoutBlockfile(merged)
	if err != nil {
		return "", nil, err
	}

	var cfg map[string]any

	if err = toml.Unmarshal(base, &cfg); err != nil {
		return "", nil, fmt.Errorf("error parsing the containerd configuration: %w", err)
	}

	criPath := filepath.Join(dir, "cri.toml")
	configPath := filepath.Join(dir, "containerd.toml")

	disabled, _ := cfg["disabled_plugins"].([]any)
	cfg["disabled_plugins"] = append(disabled, blockfilePlugin)
	cfg["imports"] = []string{criPath}

	config, err := toml.Marshal(cfg)
	if err != nil {
		return "", nil, fmt.Errorf("error writing the containerd configuration: %w", err)
	}

	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}

	for _, file := range []struct {
		path, label string
		contents    []byte
	}{
		{criPath, mergedLabel, filtered},
		{configPath, baseLabel, config},
	} {
		if err = os.WriteFile(file.path, file.contents, 0o600); err != nil {
			return "", nil, err
		}

		if err = selinux.SetLabel(file.path, file.label); err != nil {
			return "", nil, err
		}
	}

	return configPath, dropped, nil
}

// prepareBlockfile runs before the CRI containerd starts. It creates the
// blockfile scratch image when the configuration names one that is missing,
// and returns the containerd configuration to start with: the usual one, or a
// fallback without blockfile when the image cannot be made.
func prepareBlockfile(ctx context.Context) string {
	mergedPath := filepath.Join("/etc", constants.CRIConfig)

	merged, err := os.ReadFile(mergedPath)
	if err != nil {
		// containerd reports a configuration it cannot read itself.
		return constants.CRIContainerdConfig
	}

	scratch, err := blockfileScratch(merged)
	if err == nil && scratch == "" {
		return constants.CRIContainerdConfig
	}

	if err == nil {
		var created bool

		if created, err = newScratchCreator().ensure(ctx, scratch); err == nil {
			if created {
				log.Printf("blockfile: created the missing scratch image %s (%d bytes)", scratch, BlockfileScratchSize)
			}

			return constants.CRIContainerdConfig
		}
	}

	log.Printf("blockfile disabled: %s", err)

	base, baseErr := os.ReadFile(constants.CRIContainerdConfig)
	baseLabel, labelErr := selinux.GetLabel(constants.CRIContainerdConfig)
	mergedLabel, mergedLabelErr := selinux.GetLabel(mergedPath)

	if err = errors.Join(baseErr, labelErr, mergedLabelErr); err == nil {
		var (
			path    string
			dropped []string
		)

		if path, dropped, err = writeBlockfileFallback(base, merged, baseLabel, mergedLabel, blockfileFallbackDir); err == nil {
			log.Printf("blockfile disabled: the CRI starts without it and without the runtimes %v", dropped)

			return path
		}
	}

	log.Printf("blockfile disabled: could not write the fallback configuration: %s", err)

	return constants.CRIContainerdConfig
}
