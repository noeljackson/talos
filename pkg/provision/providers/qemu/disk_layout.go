// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package qemu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/siderolabs/talos/pkg/provision"
)

const (
	maxControlledDisks = 16
	maxDiskControlJSON = 1024 * 1024
)

var diskSerialPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

type persistedDiskLayout struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	provision.DiskLayout
}

func diskLayoutName(node string) string { return node + ".disks.json" }
func diskBootName(node string) string   { return node + ".disk-boot.json" }

// openDiskControlRoot pins the private state directory. Every control file and
// backing disk is a direct child, opened without following symlinks.
func openDiskControlRoot(path string) (*os.Root, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("disk control state root must be a directory, not a symlink")
	}

	return os.OpenRoot(path)
}

func readDiskControlJSON(root *os.Root, name string, target any) error {
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxDiskControlJSON {
		return errors.New("disk control file must be a bounded regular file")
	}

	decoder := json.NewDecoder(io.LimitReader(f, maxDiskControlJSON+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return err
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing disk control input")
	}

	return nil
}

func writeDiskControlJSON(root *os.Root, name string, value any) error {
	if info, err := root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("refusing to replace a non-regular disk control file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxDiskControlJSON {
		return errors.New("disk control record exceeds size limit")
	}

	temporary := ".disk-control-" + uuid.NewString()
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary) //nolint:errcheck

	_, writeErr := f.Write(data)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = root.Rename(temporary, name); err != nil {
		return err
	}

	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck

	return dir.Sync()
}

func validateDiskInventory(config *LaunchConfig) error {
	if !config.DiskLayoutControl {
		return errors.New("node did not opt into disk layout control")
	}
	count := len(config.DiskPaths)
	if count == 0 || count > maxControlledDisks || len(config.DiskSerials) != count || len(config.DiskDrivers) != count || len(config.DiskTags) != count || len(config.DiskBlockSizes) != count {
		return errors.New("invalid controlled disk inventory dimensions")
	}
	if config.NodeName == "" || filepath.Base(config.NodeName) != config.NodeName || config.NodeName == "." || config.NodeName == ".." {
		return errors.New("invalid disk control node name")
	}
	if len(config.ExtraQEMUArgs) != 0 {
		return errors.New("disk layout control cannot be combined with extra QEMU arguments")
	}
	if config.TFTPServer != "" || config.IPXEBootFileName != "" || len(config.FabricUplinks) != 0 || config.IOMMUEnabled {
		return errors.New("disk layout control cannot be combined with PXE or fabric boot topology")
	}
	if !config.BootloaderEnabled || len(config.PFlashImages) < 2 {
		return errors.New("disk layout control requires UEFI and a disk bootloader")
	}

	root, err := openDiskControlRoot(config.StatePath)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck

	seen := map[string]bool{}
	seenPaths := map[string]bool{}
	for i, path := range config.DiskPaths {
		serial := config.DiskSerials[i]
		if !diskSerialPattern.MatchString(serial) || seen[serial] || config.DiskDrivers[i] != "virtio" {
			return errors.New("controlled disks require unique safe serials and the virtio driver")
		}
		seen[serial] = true

		rel, err := filepath.Rel(config.StatePath, path)
		if err != nil || !filepath.IsLocal(rel) || filepath.Base(rel) != rel || strings.Contains(path, ",") || seenPaths[rel] {
			return errors.New("controlled backing disks must be direct children of the private state root")
		}
		seenPaths[rel] = true
		info, err := root.Lstat(rel)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("controlled backing disk must be a regular file, not a symlink")
		}
	}

	return nil
}

func diskLayoutIndexes(config *LaunchConfig, layout provision.DiskLayout) ([]int, error) {
	if !layout.DiskBootOnly {
		return nil, errors.New("disk layout changes require disk-only boot")
	}
	if len(layout.Serials) == 0 || len(layout.Serials) > maxControlledDisks {
		return nil, errors.New("invalid attached disk count")
	}
	indexes := make([]int, 0, len(layout.Serials))
	for _, serial := range layout.Serials {
		index := slices.Index(config.DiskSerials, serial)
		if index < 0 || slices.Contains(indexes, index) {
			return nil, fmt.Errorf("unknown or duplicate disk serial %q", serial)
		}
		indexes = append(indexes, index)
	}

	return indexes, nil
}

func configForDiskLayout(config *LaunchConfig) (*LaunchConfig, error) {
	if !config.DiskLayoutControl {
		return config, nil
	}
	if err := validateDiskInventory(config); err != nil {
		return nil, err
	}
	root, err := openDiskControlRoot(config.StatePath)
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck

	var layout persistedDiskLayout
	err = readDiskControlJSON(root, diskLayoutName(config.NodeName), &layout)
	if errors.Is(err, os.ErrNotExist) && config.diskGeneration == "" {
		// Also survive a launcher restart: an existing disk-only receipt means
		// losing the layout must fail closed, not restore installation media.
		var previous provision.DiskBootState
		if receiptErr := readDiskControlJSON(root, diskBootName(config.NodeName), &previous); receiptErr == nil {
			if previous.Generation != "" {
				return nil, errors.New("required disk layout missing after disk-only launch")
			}
		} else if !errors.Is(receiptErr, os.ErrNotExist) {
			return nil, receiptErr
		}

		initial := *config
		initial.diskIndexes = make([]int, len(config.DiskPaths))
		for i := range initial.diskIndexes {
			initial.diskIndexes[i] = i
		}

		return &initial, nil
	}
	if err != nil {
		return nil, err
	}
	if layout.Version != 1 {
		return nil, errors.New("unsupported disk layout version")
	}
	if _, err = uuid.Parse(layout.Generation); err != nil {
		return nil, fmt.Errorf("invalid disk layout generation: %w", err)
	}
	indexes, err := diskLayoutIndexes(config, layout.DiskLayout)
	if err != nil {
		return nil, err
	}

	// Remember that a layout is required: accidental deletion must never
	// silently restore installation media on a later reboot of this launcher.
	config.diskGeneration = layout.Generation
	selected := *config
	selected.diskIndexes = indexes
	selected.diskBootOnly = true
	selected.DiskPaths = nil
	selected.DiskDrivers = nil
	selected.DiskSerials = nil
	selected.DiskTags = nil
	selected.DiskBlockSizes = nil
	for _, i := range indexes {
		selected.DiskPaths = append(selected.DiskPaths, config.DiskPaths[i])
		selected.DiskDrivers = append(selected.DiskDrivers, config.DiskDrivers[i])
		selected.DiskSerials = append(selected.DiskSerials, config.DiskSerials[i])
		selected.DiskTags = append(selected.DiskTags, config.DiskTags[i])
		selected.DiskBlockSizes = append(selected.DiskBlockSizes, config.DiskBlockSizes[i])
	}

	return &selected, nil
}

func diskControlNode(cluster provision.Cluster, node provision.NodeInfo) (*LaunchConfig, *os.Root, error) {
	if !slices.ContainsFunc(cluster.Info().Nodes, func(candidate provision.NodeInfo) bool {
		return candidate.Name == node.Name && candidate.UUID == node.UUID
	}) || node.Name == "" || filepath.Base(node.Name) != node.Name {
		return nil, nil, errors.New("disk control node is not a member of this cluster")
	}
	path, err := cluster.StatePath()
	if err != nil {
		return nil, nil, err
	}
	root, err := openDiskControlRoot(path)
	if err != nil {
		return nil, nil, err
	}
	var config LaunchConfig
	if err = readDiskControlJSON(root, node.Name+".config", &config); err == nil {
		if config.StatePath != path || config.NodeName != node.Name || config.NodeUUID != node.UUID {
			err = errors.New("disk control configuration does not match cluster identity")
		} else {
			err = validateDiskInventory(&config)
		}
	}
	if err != nil {
		root.Close() //nolint:errcheck

		return nil, nil, err
	}

	return &config, root, nil
}

// RebootNodeWithDisks persists a validated layout and then cold-reboots the node.
// A reboot error leaves the new layout staged; it never restores media fallback.
func (p *provisioner) RebootNodeWithDisks(ctx context.Context, cluster provision.Cluster, node provision.NodeInfo, layout provision.DiskLayout) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	config, root, err := diskControlNode(cluster, node)
	if err != nil {
		return "", err
	}
	defer root.Close() //nolint:errcheck
	if _, err = diskLayoutIndexes(config, layout); err != nil {
		return "", err
	}
	generation := uuid.NewString()
	if err = writeDiskControlJSON(root, diskLayoutName(node.Name), persistedDiskLayout{Version: 1, Generation: generation, DiskLayout: layout}); err != nil {
		return "", err
	}

	return generation, p.RebootNode(ctx, cluster, node)
}

func recordDiskBootState(config *LaunchConfig, pid int, arguments []string) error {
	root, err := openDiskControlRoot(config.StatePath)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck

	return writeDiskControlJSON(root, diskBootName(config.NodeName), provision.DiskBootState{
		Generation: config.diskGeneration,
		ProcessID:  pid,
		Arguments:  arguments,
	})
}

// NodeDiskBootState verifies the recorded launch against Linux procfs. A stale
// record, exited process, or mismatched invocation is not boot evidence.
func (p *provisioner) NodeDiskBootState(ctx context.Context, cluster provision.Cluster, node provision.NodeInfo) (provision.DiskBootState, error) {
	if err := ctx.Err(); err != nil {
		return provision.DiskBootState{}, err
	}
	if runtime.GOOS != "linux" {
		return provision.DiskBootState{}, errors.New("actual disk boot inventory requires Linux procfs")
	}
	config, root, err := diskControlNode(cluster, node)
	if err != nil {
		return provision.DiskBootState{}, err
	}
	defer root.Close() //nolint:errcheck
	var boot provision.DiskBootState
	if err = readDiskControlJSON(root, diskBootName(node.Name), &boot); err != nil {
		return boot, err
	}
	selected, err := configForDiskLayout(config)
	if err != nil {
		return boot, err
	}
	if boot.Generation != selected.diskGeneration {
		return boot, errors.New("disk boot receipt does not match the current layout generation")
	}
	if err = verifyDiskBootBackingPaths(boot.Arguments, selected); err != nil {
		return boot, err
	}
	return boot, verifyDiskBootProcess(boot, config.ArchitectureData.QemuExecutable())
}

// verifyDiskBootBackingPaths binds the observed invocation to the original
// state-owned backing paths selected by configForDiskLayout. A correct serial or
// qdev ID alone does not prove that the original disk (or firmware) was restored.
func verifyDiskBootBackingPaths(arguments []string, selected *LaunchConfig) error {
	disks := make(map[string]bool, len(selected.DiskPaths))
	for i, path := range selected.DiskPaths {
		disks[fmt.Sprintf("id=virtio%d,format=raw,if=none,file=%s,cache=none", selected.diskIndexes[i], path)] = true
	}
	// Initial boot may use only the media already bound by the original config.
	// These are optional because the launcher chooses one applicable boot path.
	media := map[string]bool{}
	if !selected.diskBootOnly {
		for _, item := range []struct{ path, format string }{
			{selected.ISOPath, "id=cdrom0,file=%s,media=cdrom"},
			{selected.ExtraISOPath, "id=cdrom1,file=%s,media=cdrom"},
			{selected.USBPath, "if=none,id=stick,format=raw,read-only=on,file=%s"},
		} {
			if item.path != "" {
				media[fmt.Sprintf(item.format, item.path)] = true
			}
		}
	}
	firmware := 0
	for i, argument := range arguments {
		if argument != "-drive" {
			continue
		}
		if i+1 == len(arguments) {
			return errors.New("missing disk boot drive argument")
		}
		drive := arguments[i+1]
		switch {
		case disks[drive]:
			delete(disks, drive)
		case firmware < len(selected.PFlashImages) && drive == fmt.Sprintf("file=%s,format=raw,if=pflash", selected.PFlashImages[firmware]):
			firmware++
		case media[drive]:
			delete(media, drive)
		default:
			return fmt.Errorf("disk boot drive does not match an original backing path: %s", drive)
		}
	}
	if len(disks) != 0 || firmware != len(selected.PFlashImages) {
		return errors.New("disk boot invocation is missing original disk or firmware backing paths")
	}
	return nil
}

func verifyDiskBootProcess(boot provision.DiskBootState, executable string) error {
	if boot.ProcessID <= 0 || len(boot.Arguments) == 0 {
		return errors.New("invalid disk boot process record")
	}
	if boot.Arguments[0] != executable {
		return errors.New("disk boot record is not the configured QEMU executable")
	}
	actual, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(boot.ProcessID), "cmdline"))
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, []byte(strings.Join(boot.Arguments, "\x00")+"\x00")) {
		return errors.New("recorded disk boot inventory does not match the live process")
	}

	return nil
}
