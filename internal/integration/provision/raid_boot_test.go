// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build integration_provision

//nolint:testpackage
package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/siderolabs/talos/internal/pkg/md"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	blockpb "github.com/siderolabs/talos/pkg/machinery/api/resource/definitions/block"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/bundle"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/validation"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	storageres "github.com/siderolabs/talos/pkg/machinery/resources/storage"
	"github.com/siderolabs/talos/pkg/provision"
	"github.com/siderolabs/talos/pkg/provision/providers/qemu"
)

func TestRAIDProofDebugImageAdmission(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, image := range []string{"docker.io/library/alpine@" + digest, "registry.example.invalid:5000/proof/debug@" + digest} {
		require.NoError(t, validateRAIDDebugImage(image))
	}
	for _, image := range []string{
		"", "docker.io/library/alpine:3.23", "docker.io/library/alpine:3.23@" + digest,
		"alpine@" + digest, "library/alpine@" + digest, "index.docker.io/library/alpine@" + digest,
		"https://docker.io/library/alpine@" + digest, "user:password@docker.io/library/alpine@" + digest,
		"docker.io/library/alpine@sha256:" + strings.Repeat("A", 64),
		"docker.io/library/alpine@sha512:" + strings.Repeat("a", 128),
		"docker.io/library/alpine@sha256:" + strings.Repeat("a", 63),
		"docker.io/library/alpine@sha256:" + strings.Repeat("a", 65),
		"docker.io/library/alpine@sha256:" + strings.Repeat("g", 64),
		"docker.io/library/alpine@" + digest + " ", "docker.io/library/alpine@" + digest + "?tag=3.23",
	} {
		t.Run(image, func(t *testing.T) {
			require.Error(t, validateRAIDDebugImage(image))
			api := newRAIDDebugAPIFixture()
			require.Error(t, runRAIDDebugImage(t.Context(), &client.Client{ImageClient: api, DebugClient: api}, image, nil))
			assert.Nil(t, api.pullRequest, "invalid admission must not issue a pull")
			assert.False(t, api.debugOpened, "invalid admission must not open DebugService")
		})
	}
}

const raidTestDebugImage = "docker.io/library/alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func raidPullCompletion(image string) *machineapi.ImageServicePullResponse {
	return &machineapi.ImageServicePullResponse{Response: &machineapi.ImageServicePullResponse_Name{Name: image}}
}

func newRAIDDebugAPIFixture() *raidDebugAPIFixture {
	return &raidDebugAPIFixture{
		pull: raidPullStreamFixture{messages: []*machineapi.ImageServicePullResponse{
			{Response: &machineapi.ImageServicePullResponse_PullProgress{PullProgress: &machineapi.ImageServicePullProgress{}}},
			raidPullCompletion(raidTestDebugImage),
		}},
		debug: raidDebugStreamFixture{messages: []*machineapi.DebugContainerRunResponse{
			{Resp: &machineapi.DebugContainerRunResponse_ExitCode{ExitCode: 0}},
		}},
	}
}

// These fake service streams exercise the real request/response binding without
// creating a VM, network connection, image cache, or privileged debug process.
type raidDebugAPIFixture struct {
	machineapi.ImageServiceClient
	machineapi.DebugServiceClient
	pullRequest *machineapi.ImageServicePullRequest
	pull        raidPullStreamFixture
	debug       raidDebugStreamFixture
	pullError   error
	debugOpened bool
	debugError  error
}

func (f *raidDebugAPIFixture) Pull(_ context.Context, request *machineapi.ImageServicePullRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[machineapi.ImageServicePullResponse], error) {
	f.pullRequest = request
	return &f.pull, f.pullError
}

func (f *raidDebugAPIFixture) ContainerRun(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[machineapi.DebugContainerRunRequest, machineapi.DebugContainerRunResponse], error) {
	f.debugOpened = true
	return &f.debug, f.debugError
}

type raidPullStreamFixture struct {
	grpc.ClientStream
	messages []*machineapi.ImageServicePullResponse
	err      error
}

func (f *raidPullStreamFixture) Recv() (*machineapi.ImageServicePullResponse, error) {
	if len(f.messages) == 0 {
		if f.err != nil {
			return nil, f.err
		}
		return nil, io.EOF
	}
	message := f.messages[0]
	f.messages = f.messages[1:]
	return message, nil
}

type raidDebugStreamFixture struct {
	grpc.ClientStream
	requests []*machineapi.DebugContainerRunRequest
	messages []*machineapi.DebugContainerRunResponse
	err      error
	sendErr  error
	closeErr error
	closed   bool
}

func (f *raidDebugStreamFixture) Send(request *machineapi.DebugContainerRunRequest) error {
	f.requests = append(f.requests, request)
	return f.sendErr
}

func (f *raidDebugStreamFixture) CloseSend() error {
	f.closed = true
	return f.closeErr
}

func (f *raidDebugStreamFixture) Recv() (*machineapi.DebugContainerRunResponse, error) {
	if len(f.messages) == 0 {
		if f.err != nil {
			return nil, f.err
		}
		return nil, io.EOF
	}
	message := f.messages[0]
	f.messages = f.messages[1:]
	return message, nil
}

func TestRAIDProofDebugImageBinding(t *testing.T) {
	t.Parallel()
	api := newRAIDDebugAPIFixture()
	args := []string{"/bin/sh", "-ec", "printf marker"}
	require.NoError(t, runRAIDDebugImage(t.Context(), &client.Client{ImageClient: api, DebugClient: api}, raidTestDebugImage, args))
	require.NotNil(t, api.pullRequest)
	assert.Equal(t, raidTestDebugImage, api.pullRequest.GetImageRef())
	assert.Equal(t, common.ContainerDriver_CONTAINERD, api.pullRequest.GetContainerd().GetDriver())
	assert.Equal(t, common.ContainerdNamespace_NS_SYSTEM, api.pullRequest.GetContainerd().GetNamespace())
	require.Len(t, api.debug.requests, 1)
	spec := api.debug.requests[0].GetSpec()
	require.NotNil(t, spec)
	assert.Equal(t, api.pullRequest.GetContainerd(), spec.GetContainerd())
	assert.Equal(t, raidTestDebugImage, spec.GetImageName())
	assert.Equal(t, args, spec.GetArgs())
	assert.Equal(t, machineapi.DebugContainerRunRequestSpec_PROFILE_PRIVILEGED, spec.GetProfile())
	assert.True(t, api.debug.closed)

	for _, returned := range []string{
		"", "docker.io/library/alpine:3.23", strings.Replace(raidTestDebugImage, strings.Repeat("a", 64), strings.Repeat("b", 64), 1),
		strings.Replace(raidTestDebugImage, "library/alpine", "library/other", 1),
		strings.TrimPrefix(raidTestDebugImage, "docker.io/library/"),
	} {
		t.Run("foreign completion "+returned, func(t *testing.T) {
			api := newRAIDDebugAPIFixture()
			api.pull.messages = []*machineapi.ImageServicePullResponse{raidPullCompletion(returned)}
			require.ErrorContains(t, runRAIDDebugImage(t.Context(), &client.Client{ImageClient: api, DebugClient: api}, raidTestDebugImage, args), "wanted admitted identity")
			assert.False(t, api.debugOpened, "a mismatched pull must not execute any debug image")
		})
	}

	for _, tc := range []struct {
		name        string
		change      func(*raidDebugAPIFixture)
		debugOpened bool
	}{
		{"pull error", func(f *raidDebugAPIFixture) { f.pullError = errors.New("pull failed") }, false},
		{"pull stream error", func(f *raidDebugAPIFixture) { f.pull.err = errors.New("stream failed") }, false},
		{"missing completion", func(f *raidDebugAPIFixture) { f.pull.messages = nil }, false},
		{"nil completion", func(f *raidDebugAPIFixture) { f.pull.messages = []*machineapi.ImageServicePullResponse{nil} }, false},
		{"duplicate completion", func(f *raidDebugAPIFixture) {
			f.pull.messages = append(f.pull.messages, raidPullCompletion(raidTestDebugImage))
		}, false},
		{"debug open error", func(f *raidDebugAPIFixture) { f.debugError = errors.New("open failed") }, true},
		{"debug send error", func(f *raidDebugAPIFixture) { f.debug.sendErr = errors.New("send failed") }, true},
		{"debug close error", func(f *raidDebugAPIFixture) { f.debug.closeErr = errors.New("close failed") }, true},
		{"debug stream error", func(f *raidDebugAPIFixture) { f.debug.err = errors.New("stream failed") }, true},
		{"missing successful exit", func(f *raidDebugAPIFixture) { f.debug.messages = nil }, true},
		{"unsuccessful exit", func(f *raidDebugAPIFixture) {
			f.debug.messages[0].Resp = &machineapi.DebugContainerRunResponse_ExitCode{ExitCode: 1}
		}, true},
		{"duplicate successful exit", func(f *raidDebugAPIFixture) { f.debug.messages = append(f.debug.messages, f.debug.messages[0]) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newRAIDDebugAPIFixture()
			tc.change(api)
			require.Error(t, runRAIDDebugImage(t.Context(), &client.Client{ImageClient: api, DebugClient: api}, raidTestDebugImage, args))
			assert.Equal(t, tc.debugOpened, api.debugOpened)
		})
	}
}

func TestRAIDProofDisks(t *testing.T) {
	t.Parallel()
	original := raidProofDisks(false)
	require.Len(t, original, 5)
	serials := map[string]bool{}
	for _, disk := range original {
		assert.NotEmpty(t, disk.Serial)
		assert.False(t, serials[disk.Serial])
		serials[disk.Serial] = true
		assert.Equal(t, "virtio", disk.Driver)
		assert.EqualValues(t, 512, disk.BlockSize)
		assert.True(t, disk.SkipPreallocate)
	}
	missing := raidProofDisks(true)
	require.Len(t, missing, 4)
	assert.False(t, slices.ContainsFunc(missing, func(d *provision.Disk) bool { return d.Serial == raidBootB }))
	assert.True(t, slices.ContainsFunc(missing, func(d *provision.Disk) bool { return d.Serial == raidDecoy }))
	cloned := fixtureDisks(original)
	assert.Equal(t, original, cloned)
	cloned[0].Serial = "changed"
	assert.Equal(t, raidBootA, original[0].Serial)
	defaultDisks := fixtureDisks(nil)
	require.Len(t, defaultDisks, 1)
	assert.Equal(t, DefaultSettings.DiskGB<<30, defaultDisks[0].Size)
	assert.Empty(t, defaultDisks[0].Serial, "ordinary fixture defaults must not change")
}

func TestRAIDProofGeneratedConfiguration(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"lifecycle", "wrong-serial", "missing-serial"} {
		t.Run(variant, func(t *testing.T) {
			t.Parallel()
			provider, err := qemu.NewProvisioner(t.Context())
			require.NoError(t, err)
			defer provider.Close() //nolint:errcheck
			contract := config.TalosVersionCurrent.DisableEtcd().DisableKubernetes()
			gen, options := provider.GenOptions(provision.NetworkRequest{CIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}, contract)
			docs, err := container.New(raidProofDocuments(variant, "example.invalid/test/installer:proof")...)
			require.NoError(t, err)
			cfgBundle, err := bundle.NewBundle(append([]bundle.Option{
				bundle.WithInputOptions(&bundle.InputOptions{ClusterName: "raid-proof", Endpoint: "https://192.0.2.2:6443", KubeVersion: "1.36.0",
					GenOptions: append(gen, generate.WithVersionContract(contract), generate.WithInstallImage("example.invalid/test/installer:proof"))}),
				bundle.WithPatchControlPlane([]configpatcher.Patch{configpatcher.NewStrategicMergePatch(docs)}),
			}, options...)...)
			require.NoError(t, err)
			cfg := cfgBundle.ControlPlane()
			_, err = cfg.ValidateAsClient(validationModeMetal{})
			require.NoError(t, err, "the complete native generated fixture must validate")
			require.Len(t, cfg.RAIDArrayConfigs(), 2)
			install := cfg.UnattendedInstallConfig()
			require.NotNil(t, install)
			assert.Equal(t, "example.invalid/test/installer:proof", install.InstallerImage())
			require.NotNil(t, install.RebootAfterInstall())
			assert.False(t, *install.RebootAfterInstall(), "fixture must observe initial raw-kernel security before its controlled cold boot")
			require.NotNil(t, cfg.SecurityProfileConfig())
			assert.True(t, cfg.SecurityProfileConfig().WorkloadIsolation())
			assert.Equal(t, fmt.Sprintf("%q in disk.symlinks", md.DevicePath("boot")), install.VolumeSelector().String(), "native default /dev/vda selector must be replaced")
			for _, array := range cfg.RAIDArrayConfigs() {
				assert.Equal(t, storageres.MDLevelRAID1, array.RAIDLevel())
				metadata := storageres.MDMetadata10
				if array.Name() == "data" {
					metadata = storageres.MDMetadata12
				}
				assert.Equal(t, metadata, array.RAIDMetadata())
				for _, disk := range raidProofDisks(variant == "missing-serial") {
					want := disk.Serial == raidDataA || disk.Serial == raidDataB
					if array.Name() == "boot" {
						want = disk.Serial == raidBootA || (variant != "wrong-serial" && disk.Serial == raidBootB)
					}
					matches, err := array.Provisioning().VolumeSelector().EvalBool(celenv.DiskLocator(), map[string]any{"disk": &blockpb.DiskSpec{Serial: disk.Serial}})
					require.NoError(t, err)
					assert.Equal(t, want, matches, "%s must select exact disjoint serials, including decoy exclusion", array.Name())
				}
			}
			if variant == "lifecycle" {
				require.Len(t, cfg.UserVolumeConfigs(), 1)
				assert.Equal(t, block.FilesystemTypeXFS, cfg.UserVolumeConfigs()[0].Filesystem().Type())
			} else {
				assert.Empty(t, cfg.UserVolumeConfigs(), "negative fixtures must leave the data mirror unformatted")
			}
		})
	}
}

type validationModeMetal struct{}

var _ validation.RuntimeMode = validationModeMetal{}

func (validationModeMetal) RequiresInstall() bool { return true }
func (validationModeMetal) InContainer() bool     { return false }
func (validationModeMetal) String() string        { return "metal" }

func TestRAIDProofBootEvidence(t *testing.T) {
	t.Parallel()
	disks := raidProofDisks(false)
	serials := []string{raidDataB, raidBootB, raidDataA, raidDecoy}
	valid := []string{"qemu-system-x86_64", "-machine", "q35,accel=kvm", "-boot", "strict=on,reboot-timeout=5000",
		"-drive", "file=/fixture/code,format=raw,if=pflash", "-drive", "file=/fixture/vars,format=raw,if=pflash",
		"-device", "virtio-net-pci,netdev=net0,romfile="}
	for position, serial := range serials {
		i := slices.IndexFunc(disks, func(d *provision.Disk) bool { return d.Serial == serial })
		valid = append(valid, "-drive", fmt.Sprintf("id=virtio%d,format=raw,if=none,file=/fixture/disk%d", i, i),
			"-device", fmt.Sprintf("virtio-blk-pci,drive=virtio%d,serial=%s,id=talos-disk%d,bootindex=%d", i, serial, i, position+1))
	}
	require.NoError(t, validateRAIDBootArguments(valid, disks, serials))
	for _, tc := range []struct{ name, old, replacement string }{
		{"software emulation", "accel=kvm", "accel=tcg"},
		{"non-strict firmware", "strict=on", "order=c"},
		{"network ROM", "romfile=", "romfile=pxe.rom"},
		{"network boot index", "romfile=", "romfile=,bootindex=1"},
		{"wrong serial", raidBootB, "unknown"},
		{"stale qdev identity", "id=talos-disk1", "id=talos-disk0"},
		{"wrong backend", "drive=virtio1", "drive=virtio0"},
		{"wrong boot order", "bootindex=1", "bootindex=9"},
		{"duplicate property", "bootindex=1", "bootindex=1,bootindex=1"},
		{"config media", "id=virtio3,", "id=cdrom0,"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := slices.Clone(valid)
			for i := range args {
				args[i] = strings.ReplaceAll(args[i], tc.old, tc.replacement)
			}
			require.Error(t, validateRAIDBootArguments(args, disks, serials))
		})
	}
	for _, forbidden := range []string{"-kernel", "-initrd", "-append", "-cdrom", "-option-rom", "talos.config=http://fixture", "type=11,value=config"} {
		t.Run(forbidden, func(t *testing.T) {
			require.Error(t, validateRAIDBootArguments(append(slices.Clone(valid), forbidden), disks, serials))
		})
	}
	require.Error(t, validateRAIDBootArguments(valid, disks, []string{raidBootB, raidDataB, raidDataA, raidDecoy}))
	require.Error(t, validateRAIDBootArguments(append(slices.Clone(valid), "-drive", "id=virtio0,if=none,file=/unexpected"), disks, serials))
	require.Error(t, validateRAIDBootArguments(append(slices.Clone(valid), "-device", "e1000,netdev=net1"), disks, serials))
}

func raidInitialArgumentFixture(disks []*provision.Disk) []string {
	args := []string{"qemu-system-x86_64", "-machine", "q35,accel=kvm", "-boot", "order=cd,reboot-timeout=5000",
		"-kernel", "/fixture/vmlinuz", "-initrd", "/fixture/initramfs", "-append", "enforcing=1 talos.config=http://fixture",
		"-drive", "file=/fixture/code,format=raw,if=pflash", "-drive", "file=/fixture/vars,format=raw,if=pflash",
		"-device", "virtio-net-pci,netdev=net0", "-smbios", "type=11,value=config"}
	for i, disk := range disks {
		args = append(args, "-drive", fmt.Sprintf("id=virtio%d,format=raw,if=none,file=/fixture/disk%d", i, i),
			"-device", fmt.Sprintf("virtio-blk-pci,drive=virtio%d,serial=%s,id=talos-disk%d", i, disk.Serial, i))
	}
	return args
}

func TestRAIDProofInitialLaunchEvidence(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		disks := raidProofDisks(missing)
		require.NoError(t, validateRAIDInitialBootArguments(raidInitialArgumentFixture(disks), disks))
	}
	disks := raidProofDisks(false)
	valid := raidInitialArgumentFixture(disks)
	require.Error(t, validateRAIDInitialBootArguments(raidInitialArgumentFixture(raidProofDisks(true)), disks),
		"wrong-serial fixture must not accept an actually missing BOOT-B")
	for _, tc := range []struct{ name, old, replacement string }{
		{"wrong serial", raidBootB, "unknown"},
		{"duplicate serial", raidBootB, raidBootA},
		{"wrong backend", "drive=virtio1", "drive=virtio0"},
		{"wrong qdev", "id=talos-disk1", "id=talos-disk0"},
		{"software emulation", "accel=kvm", "accel=tcg"},
		{"no raw kernel", "-kernel", "-not-kernel"},
		{"no initramfs", "-initrd", "-not-initrd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := slices.Clone(valid)
			for i := range args {
				args[i] = strings.ReplaceAll(args[i], tc.old, tc.replacement)
			}
			require.Error(t, validateRAIDInitialBootArguments(args, disks))
		})
	}
	require.Error(t, validateRAIDInitialBootArguments(valid[:len(valid)-4], disks), "missing decoy must not pass")
	require.Error(t, validateRAIDInitialBootArguments(append(slices.Clone(valid), valid[len(valid)-4:]...), disks), "duplicate physical attachment must not pass")
}

func raidPhysicalDiskFixture(expected []*provision.Disk) []*block.Disk {
	disks := make([]*block.Disk, len(expected))
	for i, want := range expected {
		disk := block.NewDisk(block.NamespaceName, fmt.Sprintf("vd%c", 'a'+i))
		disk.TypedSpec().Serial = want.Serial
		disk.TypedSpec().DevPath = "/dev/" + disk.Metadata().ID()
		disk.TypedSpec().Size = want.Size
		disks[i] = disk
	}
	return disks
}

func TestRAIDProofPhysicalInventory(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		expected := raidProofDisks(missing)
		disks := raidPhysicalDiskFixture(expected)
		data := block.NewDisk(block.NamespaceName, "md127")
		data.TypedSpec().SecondaryDisks = []string{"vdc", "vdd"}
		disks = append(disks, data)
		require.NoError(t, validateRAIDPhysicalDisks(disks, expected), "MD virtual devices are not extra physical disks")
	}
	expected := raidProofDisks(false)
	require.Error(t, validateRAIDPhysicalDisks(raidPhysicalDiskFixture(raidProofDisks(true)), expected),
		"wrong-serial fixture must not pass with BOOT-B undiscovered")
	require.Error(t, validateRAIDPhysicalDisks(nil, expected), "empty discovery is not safe-installation evidence")
	for i, disk := range expected {
		t.Run("missing "+disk.Serial, func(t *testing.T) {
			disks := raidPhysicalDiskFixture(expected)
			require.Error(t, validateRAIDPhysicalDisks(append(disks[:i], disks[i+1:]...), expected))
		})
	}
	for _, tc := range []struct {
		name   string
		change func([]*block.Disk)
	}{
		{"unknown serial", func(d []*block.Disk) { d[1].TypedSpec().Serial = "unknown" }},
		{"duplicate serial", func(d []*block.Disk) { d[1].TypedSpec().Serial = raidBootA }},
		{"read-only", func(d []*block.Disk) { d[1].TypedSpec().Readonly = true }},
		{"wrong size", func(d []*block.Disk) { d[1].TypedSpec().Size = 0 }},
		{"missing path", func(d []*block.Disk) { d[1].TypedSpec().DevPath = "" }},
		{"not physical", func(d []*block.Disk) { d[1].TypedSpec().SecondaryDisks = []string{"vda"} }},
		{"CD-ROM", func(d []*block.Disk) { d[1].TypedSpec().CDROM = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disks := raidPhysicalDiskFixture(expected)
			tc.change(disks)
			require.Error(t, validateRAIDPhysicalDisks(disks, expected))
		})
	}
	extra := block.NewDisk(block.NamespaceName, "vdf")
	extra.TypedSpec().Serial = "unexpected"
	require.Error(t, validateRAIDPhysicalDisks(append(raidPhysicalDiskFixture(expected), extra), expected))
}
