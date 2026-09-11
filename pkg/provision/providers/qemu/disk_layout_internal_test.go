// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//nolint:testpackage
package qemu

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/pkg/provision"
)

func diskControlFixture(t *testing.T) *LaunchConfig {
	t.Helper()
	root := t.TempDir()
	config := &LaunchConfig{
		StatePath: root, NodeName: "node", NodeUUID: uuid.New(),
		DiskLayoutControl: true, BootloaderEnabled: true,
		ArchitectureData: ArchAmd64, controller: NewController(),
		MemSize: 4096, VCPUCount: 4,
		Network:         networkConfig{networkConfigBase: networkConfigBase{CIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}},
		DiskSerials:     []string{"BOOT-A", "BOOT-B", "DATA-A", "DATA-B", "DECOY"},
		PFlashImages:    []string{filepath.Join(root, "code.fd"), filepath.Join(root, "vars.fd")},
		KernelImagePath: "/initial/kernel", InitrdPath: "/initial/initramfs",
		ISOPath: "/initial/installer.iso", USBPath: "/initial/installer.usb",
		UKIPath: "/initial/uki", ExtraISOPath: "/initial/config.iso",
		sdStubExtraCmdlineConfig: " talos.config=http://test/config.yaml",
	}
	for i := range config.DiskSerials {
		path := filepath.Join(root, config.DiskSerials[i]+".disk")
		f, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, f.Truncate(1024*1024))
		require.NoError(t, f.Close())
		config.DiskPaths = append(config.DiskPaths, path)
		config.DiskDrivers = append(config.DiskDrivers, "virtio")
		config.DiskBlockSizes = append(config.DiskBlockSizes, 512)
		config.DiskTags = append(config.DiskTags, "")
	}
	for _, path := range config.PFlashImages {
		require.NoError(t, os.WriteFile(path, []byte("preserved firmware"), 0o600))
	}

	return config
}

func persistTestLayout(t *testing.T, config *LaunchConfig, serials []string) string {
	t.Helper()
	root, err := openDiskControlRoot(config.StatePath)
	require.NoError(t, err)
	defer root.Close() //nolint:errcheck
	gen := uuid.NewString()
	require.NoError(t, writeDiskControlJSON(root, diskLayoutName(config.NodeName), persistedDiskLayout{
		Version: 1, Generation: gen,
		DiskLayout: provision.DiskLayout{Serials: serials, DiskBootOnly: true},
	}))

	return gen
}

func TestDiskLayoutDefaultUnchanged(t *testing.T) {
	t.Parallel()
	config := &LaunchConfig{}
	actual, err := configForDiskLayout(config)
	require.NoError(t, err)
	assert.Same(t, config, actual, "ordinary launch must not read disk control files")

	config = diskControlFixture(t)
	initial, err := configForDiskLayout(config)
	require.NoError(t, err)
	assert.False(t, initial.diskBootOnly)
	assert.Equal(t, []int{0, 1, 2, 3, 4}, initial.diskIndexes)
	args, err := prepareQEMUArgs(initial)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(args, " "), "file=/initial/installer.iso")
	assert.Contains(t, strings.Join(args, " "), "file=/initial/config.iso")
}

func TestDiskLayoutReorderedDiskOnlyInvocation(t *testing.T) {
	t.Parallel()
	config := diskControlFixture(t)
	// Resetting the variable store would truncate this sentinel to zero bytes.
	config.PFlashSpec = []PFlash{{}, {}}
	gen := persistTestLayout(t, config, []string{"DATA-B", "BOOT-B", "DECOY", "DATA-A"})
	selected, err := configForDiskLayout(config)
	require.NoError(t, err)
	assert.Equal(t, gen, selected.diskGeneration)
	assert.Equal(t, []int{3, 1, 4, 2}, selected.diskIndexes)
	assert.Equal(t, []string{"BOOT-A", "BOOT-B", "DATA-A", "DATA-B", "DECOY"}, config.DiskSerials)
	args, err := prepareQEMUArgs(selected)
	require.NoError(t, err)
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"-kernel", "-initrd", "-append", "/initial/", "talos.config=", "type=11", "serial=BOOT-A"} {
		assert.NotContains(t, joined, forbidden)
	}
	assert.Contains(t, joined, "strict=on,reboot-timeout=5000")
	assert.NotContains(t, joined, "order=")
	assert.Contains(t, joined, "host_mtu=0,romfile=")
	assert.Contains(t, joined, "serial=BOOT-B,id=talos-disk1")
	assert.Contains(t, joined, "serial=DATA-B,id=talos-disk3")
	assert.Contains(t, joined, "id=talos-disk3,bootindex=1")
	assert.Contains(t, joined, "id=talos-disk1,bootindex=2")
	assert.Less(t, strings.Index(joined, "serial=DATA-B"), strings.Index(joined, "serial=BOOT-B"))
	vars, err := os.ReadFile(config.PFlashImages[1])
	require.NoError(t, err)
	assert.Equal(t, "preserved firmware", string(vars))

	// A later launch rereads the persisted attachment/order, not the previous
	// selected slices. The absent member can return without losing its identity.
	persistTestLayout(t, config, []string{"BOOT-A", "DATA-A", "BOOT-B", "DATA-B", "DECOY"})
	rejoined, err := configForDiskLayout(config)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 2, 1, 3, 4}, rejoined.diskIndexes)
	require.NoError(t, os.Remove(filepath.Join(config.StatePath, diskLayoutName(config.NodeName))))
	_, err = configForDiskLayout(config)
	require.Error(t, err, "missing required layout must not restore media fallback")
	require.NoError(t, recordDiskBootState(rejoined, 123, []string{"qemu"}))
	config.diskGeneration = "" // simulate restarting the launcher from its original config
	_, err = configForDiskLayout(config)
	require.ErrorContains(t, err, "required disk layout missing")
}

func TestDiskLayoutRejectsInvalidSelectors(t *testing.T) {
	t.Parallel()
	config := diskControlFixture(t)
	for _, tc := range []struct {
		name   string
		layout provision.DiskLayout
	}{
		{"unknown", provision.DiskLayout{Serials: []string{"UNKNOWN"}, DiskBootOnly: true}},
		{"duplicate", provision.DiskLayout{Serials: []string{"BOOT-A", "BOOT-A"}, DiskBootOnly: true}},
		{"empty", provision.DiskLayout{DiskBootOnly: true}},
		{"host path", provision.DiskLayout{Serials: []string{config.DiskPaths[0]}, DiskBootOnly: true}},
		{"media allowed", provision.DiskLayout{Serials: []string{"BOOT-A"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := diskLayoutIndexes(config, tc.layout)
			require.Error(t, err)
		})
	}
}

func TestDiskLayoutRejectsInvalidInventory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*LaunchConfig)
	}{
		{"not opted in", func(c *LaunchConfig) { c.DiskLayoutControl = false }},
		{"missing serial", func(c *LaunchConfig) { c.DiskSerials[0] = "" }},
		{"duplicate serial", func(c *LaunchConfig) { c.DiskSerials[0] = c.DiskSerials[1] }},
		{"unsafe serial", func(c *LaunchConfig) { c.DiskSerials[0] = "BOOT-A,drive=other" }},
		{"wrong driver", func(c *LaunchConfig) { c.DiskDrivers[0] = "nvme" }},
		{"truncated inventory", func(c *LaunchConfig) { c.DiskTags = nil }},
		{"external disk", func(c *LaunchConfig) { c.DiskPaths[0] = "/dev/sda" }},
		{"extra arguments", func(c *LaunchConfig) { c.ExtraQEMUArgs = []string{"-kernel", "/another"} }},
		{"no bootloader", func(c *LaunchConfig) { c.BootloaderEnabled = false }},
		{"no UEFI", func(c *LaunchConfig) { c.PFlashImages = nil }},
		{"PXE", func(c *LaunchConfig) { c.TFTPServer = "192.0.2.1" }},
		{"extra IOMMU NIC", func(c *LaunchConfig) { c.IOMMUEnabled = true }},
		{"duplicate backing", func(c *LaunchConfig) { c.DiskPaths[1] = c.DiskPaths[0] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := diskControlFixture(t)
			tc.change(config)
			require.Error(t, validateDiskInventory(config))
		})
	}
}

func TestDiskControlRejectsSymlinksAndMalformedFiles(t *testing.T) {
	t.Parallel()
	config := diskControlFixture(t)
	root, err := openDiskControlRoot(config.StatePath)
	require.NoError(t, err)
	defer root.Close() //nolint:errcheck
	out := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(out, []byte("untouched"), 0o600))
	name := diskLayoutName(config.NodeName)
	require.NoError(t, os.Symlink(out, filepath.Join(config.StatePath, name)))
	require.Error(t, writeDiskControlJSON(root, name, persistedDiskLayout{}))
	_, err = configForDiskLayout(config)
	require.Error(t, err)
	contents, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "untouched", string(contents))
	require.NoError(t, root.Remove(name))

	for _, invalid := range []string{
		`{"version":1,"generation":"invalid","serials":["BOOT-A"],"diskBootOnly":true}`,
		`{"version":2,"generation":"` + uuid.NewString() + `","serials":["BOOT-A"],"diskBootOnly":true}`,
		`{"hostPath":"/dev/sda"}`,
		`{} {}`,
		strings.Repeat(" ", maxDiskControlJSON+1),
	} {
		require.NoError(t, os.WriteFile(filepath.Join(config.StatePath, name), []byte(invalid), 0o600))
		_, err = configForDiskLayout(config)
		require.Error(t, err)
	}

	require.NoError(t, os.Remove(config.DiskPaths[0]))
	require.NoError(t, os.Symlink(out, config.DiskPaths[0]))
	require.Error(t, validateDiskInventory(config))
	linkedRoot := filepath.Join(t.TempDir(), "state")
	require.NoError(t, os.Symlink(config.StatePath, linkedRoot))
	_, err = openDiskControlRoot(linkedRoot)
	require.Error(t, err)
}

func TestDiskBootStateRecordsActualInvocation(t *testing.T) {
	t.Parallel()
	config := diskControlFixture(t)
	config.diskGeneration = uuid.NewString()
	args := []string{"qemu-system-x86_64", "-device", "virtio-blk-pci,serial=BOOT-B,id=talos-disk1"}
	require.NoError(t, recordDiskBootState(config, 123, args))
	data, err := os.ReadFile(filepath.Join(config.StatePath, diskBootName(config.NodeName)))
	require.NoError(t, err)
	var boot provision.DiskBootState
	require.NoError(t, json.Unmarshal(data, &boot))
	assert.Equal(t, config.diskGeneration, boot.Generation)
	assert.Equal(t, 123, boot.ProcessID)
	assert.Equal(t, args, boot.Arguments)
}

func TestDiskBootBackingPaths(t *testing.T) {
	t.Parallel()
	config := diskControlFixture(t)
	initial, err := configForDiskLayout(config)
	require.NoError(t, err)
	args, err := prepareQEMUArgs(initial)
	require.NoError(t, err)
	require.NoError(t, verifyDiskBootBackingPaths(args, initial), "initial media must remain bound to the original config")

	persistTestLayout(t, config, []string{"DATA-B", "BOOT-B", "DECOY", "DATA-A"})
	selected, err := configForDiskLayout(config)
	require.NoError(t, err)
	args, err = prepareQEMUArgs(selected)
	require.NoError(t, err)
	require.NoError(t, verifyDiskBootBackingPaths(args, selected), "reordering must preserve original disk/backend and firmware paths")

	for _, tc := range []struct {
		name   string
		change func([]string) []string
	}{
		{"swapped disk files", func(args []string) []string {
			replacer := strings.NewReplacer(selected.DiskPaths[0], selected.DiskPaths[1], selected.DiskPaths[1], selected.DiskPaths[0])
			for i := range args {
				args[i] = replacer.Replace(args[i])
			}
			return args
		}},
		{"replacement disk file", func(args []string) []string {
			for i := range args {
				args[i] = strings.ReplaceAll(args[i], selected.DiskPaths[0], filepath.Join(config.StatePath, "replacement.disk"))
			}
			return args
		}},
		{"swapped firmware files", func(args []string) []string {
			replacer := strings.NewReplacer(config.PFlashImages[0], config.PFlashImages[1], config.PFlashImages[1], config.PFlashImages[0])
			for i := range args {
				args[i] = replacer.Replace(args[i])
			}
			return args
		}},
		{"replacement firmware file", func(args []string) []string {
			for i := range args {
				args[i] = strings.ReplaceAll(args[i], config.PFlashImages[1], filepath.Join(config.StatePath, "replacement-vars.fd"))
			}
			return args
		}},
		{"missing firmware", func(args []string) []string {
			for i, arg := range args {
				if strings.Contains(arg, "file="+config.PFlashImages[1]+",") {
					return append(args[:i-1], args[i+1:]...)
				}
			}
			panic("fixture firmware not found")
		}},
		{"missing disk", func(args []string) []string {
			for i, arg := range args {
				if strings.Contains(arg, "file="+selected.DiskPaths[0]+",") {
					return append(args[:i-1], args[i+1:]...)
				}
			}
			panic("fixture disk not found")
		}},
		{"duplicate disk", func(args []string) []string {
			for _, arg := range args {
				if strings.Contains(arg, "file="+selected.DiskPaths[0]+",") {
					return append(args, "-drive", arg)
				}
			}
			panic("fixture disk not found")
		}},
		{"duplicate firmware", func(args []string) []string {
			return append(args, "-drive", "file="+config.PFlashImages[1]+",format=raw,if=pflash")
		}},
		{"fallback media", func(args []string) []string {
			return append(args, "-drive", "id=cdrom0,file="+config.ISOPath+",media=cdrom")
		}},
		{"truncated drive argument", func(args []string) []string { return append(args, "-drive") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, verifyDiskBootBackingPaths(tc.change(slices.Clone(args)), selected))
		})
	}
}

func TestDiskBootStateRejectsStaleOrDifferentProcess(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("live inventory uses Linux procfs")
	}
	// Inspect only this test process. No subprocess, QEMU, or network is used.
	self := provision.DiskBootState{ProcessID: os.Getpid(), Arguments: os.Args}
	require.NoError(t, verifyDiskBootProcess(self, os.Args[0]))
	require.Error(t, verifyDiskBootProcess(self, "not-this-executable"))
	for _, tc := range []struct {
		name   string
		change func(*provision.DiskBootState)
	}{
		{"invalid pid", func(b *provision.DiskBootState) { b.ProcessID = 0 }},
		{"absent process", func(b *provision.DiskBootState) { b.ProcessID = 2147483647 }},
		{"empty invocation", func(b *provision.DiskBootState) { b.Arguments = nil }},
		{"different invocation", func(b *provision.DiskBootState) { b.Arguments = append(slices.Clone(b.Arguments), "not-present") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := self
			tc.change(&changed)
			require.Error(t, verifyDiskBootProcess(changed, os.Args[0]))
		})
	}
}
