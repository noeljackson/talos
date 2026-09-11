// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build integration_provision

package provision

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/distribution/reference"
	"github.com/google/uuid"
	"github.com/siderolabs/go-procfs/procfs"
	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"

	"github.com/siderolabs/talos/cmd/talosctl/pkg/mgmt/helpers"
	"github.com/siderolabs/talos/internal/pkg/md"
	"github.com/siderolabs/talos/pkg/images"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/config"
	configconfig "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	blockcfg "github.com/siderolabs/talos/pkg/machinery/config/types/block"
	runtimecfg "github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	storagecfg "github.com/siderolabs/talos/pkg/machinery/config/types/storage"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/cluster"
	configres "github.com/siderolabs/talos/pkg/machinery/resources/config"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	storageres "github.com/siderolabs/talos/pkg/machinery/resources/storage"
	"github.com/siderolabs/talos/pkg/provision"
)

var (
	raidProofEnabled = flag.Bool("talos.provision.raid-proof", false, "enable isolated native QEMU RAID cold-boot proof fixtures")
	raidDebugImage   = flag.String("talos.provision.raid-debug-image", "", "reviewed canonical name@sha256:digest debug image for native RAID proof (required with raid-proof)")
)

const (
	raidBootA        = "RAID-SYSTEM-A"
	raidBootB        = "RAID-SYSTEM-B"
	raidDataA        = "RAID-DATA-A"
	raidDataB        = "RAID-DATA-B"
	raidDecoy        = "RAID-SYSTEM-B-decoy"
	raidDataPath     = "/var/mnt/raid-data"
	raidSystemMarker = "/var/raid-proof-marker"
	raidDataMarker   = raidDataPath + "/raid-proof-marker"
)

// RAIDBootSuite proves serial-selected mirrors with cold-boot device loss and
// rejoin. It does not claim online hot-unplug or physical-hardware qualification.
type RAIDBootSuite struct {
	BaseSuite
	variant                                          string
	privateRoot                                      string
	debugImage                                       string
	disks                                            []*provision.Disk
	node                                             provision.NodeInfo
	diskControl                                      provision.DiskLayoutProvisioner
	bootUUID, dataUUID, dataFilesystemUUID, identity string
}

func (suite *RAIDBootSuite) SuiteName() string {
	return "provision.RAIDBootSuite." + suite.variant + "-TR3"
}

func (suite *RAIDBootSuite) SetupSuite() {
	suite.BaseSuite.SetupSuite()
	if !*raidProofEnabled {
		suite.T().Skip("requires explicit -talos.provision.raid-proof")
	}
	// Admission precedes any VM creation or private state allocation. Keep the
	// admitted identity in the suite, not a mutable tag or a later flag lookup.
	suite.Require().NoError(validateRAIDDebugImage(*raidDebugImage))
	suite.debugImage = *raidDebugImage
	suite.T().Logf("admitted RAID debug image=%s native CIDR=%s", suite.debugImage, DefaultSettings.CIDR)
	suite.Require().Equal("linux", runtime.GOOS, "RAID proof requires Linux procfs and KVM")
	suite.Require().NoError(unix.Access("/dev/kvm", unix.R_OK|unix.W_OK), "RAID proof requires accessible KVM, not software emulation")
	// Keep AF_UNIX socket paths short and full resync off /tmp's tmpfs. This
	// directory is created by this fixture and never contains user credentials.
	var err error
	suite.privateRoot, err = os.MkdirTemp("/var/tmp", "talos-raid-")
	suite.Require().NoError(err)
	suite.T().Cleanup(func() { suite.Assert().NoError(os.RemoveAll(suite.privateRoot)) })
	var space unix.Statfs_t
	suite.Require().NoError(unix.Statfs(suite.privateRoot, &space))
	suite.Require().GreaterOrEqual(space.Bavail*uint64(space.Bsize), uint64(32<<30), "full mirror resync requires 32 GiB free disk space")
}

func raidSerialExpression(serials ...string) cel.Expression {
	clauses := make([]string, len(serials))
	for i, serial := range serials {
		clauses[i] = "disk.serial == " + strconv.Quote(serial)
	}

	return cel.MustExpression(cel.ParseBooleanExpression(strings.Join(clauses, " || "), celenv.DiskLocator()))
}

func raidProofDisks(missing bool) []*provision.Disk {
	disks := []*provision.Disk{}
	for _, serial := range []string{raidBootA, raidBootB, raidDataA, raidDataB, raidDecoy} {
		if missing && serial == raidBootB {
			continue
		}
		size := uint64(2 << 30)
		if serial == raidBootA || serial == raidBootB {
			size = 12 << 30
		}
		disks = append(disks, &provision.Disk{Size: size, Driver: "virtio", BlockSize: 512, Serial: serial, SkipPreallocate: true})
	}

	return disks
}

func raidProofDocuments(variant, installer string) []configconfig.Document {
	boot := storagecfg.NewRAIDArrayConfigV1Alpha1()
	boot.MetaName = "boot"
	boot.Level = storageres.MDLevelRAID1
	boot.MetadataFormat = storageres.MDMetadata10
	second := raidBootB
	if variant == "wrong-serial" {
		second += "-wrong"
	}
	boot.ProvisioningSpec.RAIDVolumeSelector.Match = raidSerialExpression(raidBootA, second)
	data := storagecfg.NewRAIDArrayConfigV1Alpha1()
	data.MetaName = "data"
	data.Level = storageres.MDLevelRAID1
	data.MetadataFormat = storageres.MDMetadata12
	data.ProvisioningSpec.RAIDVolumeSelector.Match = raidSerialExpression(raidDataA, raidDataB)
	install := runtimecfg.NewUnattendedInstallConfigV1Alpha1()
	install.Installer.Image = installer
	// Observe the initial raw-kernel security state before the fixture itself
	// performs the first strictly disk-only boot of the installed UKI.
	install.Reboot = new(false)
	install.ProvisioningSpec.DiskSelector.Match = cel.MustExpression(cel.ParseBooleanExpression(
		strconv.Quote(md.DevicePath("boot"))+" in disk.symlinks", celenv.DiskLocator(),
	))
	security := runtimecfg.NewSecurityProfileConfigV1Alpha1()
	security.WorkloadIsolationEnabled = new(true)
	documents := []configconfig.Document{boot, data, install, security}
	if variant == "lifecycle" {
		// This disposable user filesystem is test-only. The production data
		// mirror's unformatted provisioning contract is not changed.
		volume := blockcfg.NewUserVolumeConfigV1Alpha1()
		volume.MetaName = "raid-data"
		volume.VolumeType = new(block.VolumeTypeDisk)
		volume.ProvisioningSpec.DiskSelectorSpec.Match = cel.MustExpression(cel.ParseBooleanExpression(
			strconv.Quote(md.DevicePath("data"))+" in disk.symlinks", celenv.DiskLocator(),
		))
		volume.FilesystemSpec.FilesystemType = block.FilesystemTypeXFS
		documents = append(documents, volume)
	}

	return documents
}

// TestProof is one lifecycle per isolated cluster, or one fresh fail-closed
// negative fixture. None of these paths changes the host network outside the
// existing native provisioner topology.
func (suite *RAIDBootSuite) TestProof() {
	installer := fmt.Sprintf("%s/%s:%s", DefaultSettings.TargetInstallImageRegistry,
		images.DefaultInstallerImageName, DefaultSettings.CurrentVersion) //nolint:staticcheck // native test installer
	suite.disks = raidProofDisks(suite.variant == "missing-serial")
	documents, err := container.New(raidProofDocuments(suite.variant, installer)...)
	suite.Require().NoError(err)
	suite.setupCluster(clusterOptions{
		ClusterName: "raid-" + suite.variant, PrivateStateRoot: suite.privateRoot,
		ControlplaneNodes: 1, WorkerNodes: 0, MemoryMB: 4096,
		NodeDisks: suite.disks, QEMUDiskLayoutControl: true,
		InjectBootKernelArgs: procfs.NewCmdline("enforcing=1"),
		SourceKernelPath:     helpers.ArtifactPath(constants.KernelAssetWithArch),
		SourceInitramfsPath:  helpers.ArtifactPath(constants.InitramfsAssetWithArch),
		SourceInstallerImage: installer, SourceVersion: DefaultSettings.CurrentVersion,
		SourceK8sVersion:          constants.DefaultKubernetesVersion,
		VersionContract:           config.TalosVersionCurrent.DisableEtcd().DisableKubernetes(),
		ConfigPatchesControlPlane: []configpatcher.Patch{configpatcher.NewStrategicMergePatch(documents)},
		SkipBootstrapAndHealth:    suite.variant != "lifecycle",
	})
	suite.Require().Len(suite.Cluster.Info().Nodes, 1)
	suite.node = suite.Cluster.Info().Nodes[0]
	suite.Require().Equal(int64(4<<30), suite.node.Memory)
	var ok bool
	suite.diskControl, ok = suite.provisioner.(provision.DiskLayoutProvisioner)
	suite.Require().True(ok, "native disk-control capability is required")
	c, err := suite.clusterAccess.Client()
	suite.Require().NoError(err)
	ctx := client.WithNode(suite.ctx, suite.node.IPs[0].String())
	suite.Require().EventuallyWithT(func(collect *assert.CollectT) {
		security, err := safe.StateGetByID[*runtimeres.SecurityState](ctx, c.COSI, runtimeres.SecurityStateID)
		if assert.NoError(collect, err) {
			assert.Equal(collect, runtimeres.SELinuxStateEnforcing, security.TypedSpec().SELinuxState)
			assert.False(collect, security.TypedSpec().BootedWithUKI, "initial boot must be the explicitly enforcing raw kernel")
		}
	}, 3*time.Minute, time.Second)
	initialCmdline, err := readRAIDText(ctx, c, "/proc/cmdline")
	suite.Require().NoError(err)
	suite.Require().Contains(strings.Fields(initialCmdline), "enforcing=1")
	suite.assertRAIDSecurity(ctx, c)
	if suite.variant != "lifecycle" {
		suite.assertInitialRAIDInventory(ctx, c)
		suite.assertFreshSelectorFailure(ctx, c)

		return
	}

	boot := suite.waitRAID(ctx, c, "boot", []string{raidBootA, raidBootB})
	data := suite.waitRAID(ctx, c, "data", []string{raidDataA, raidDataB})
	suite.bootUUID, suite.dataUUID = boot.uuid, data.uuid
	suite.Require().NotEqual(suite.bootUUID, suite.dataUUID)
	suite.assertDataMount(ctx, c, data.device)
	identity, err := safe.StateGetByID[*cluster.Identity](ctx, c.COSI, cluster.LocalIdentity)
	suite.Require().NoError(err)
	suite.identity = identity.TypedSpec().NodeID
	suite.Require().NotEmpty(suite.identity)
	marker := uuid.NewString()
	suite.writeRAIDMarkers(ctx, c, marker)

	// Qualify disk-only boot first, with all members still present.
	suite.coldBootRAID(ctx, c, []string{raidBootA, raidBootB, raidDataA, raidDataB, raidDecoy})
	suite.assertRAIDPersistence(ctx, c, marker)
	for _, absent := range []string{raidBootA, raidBootB} {
		survivor := raidBootA
		if absent == raidBootA {
			survivor = raidBootB
		}
		suite.T().Logf("cold boot with system member %s physically absent", absent)
		suite.coldBootRAID(ctx, c, []string{survivor, raidDataA, raidDataB, raidDecoy})
		suite.waitRAID(ctx, c, "boot", []string{survivor})
		suite.assertRAIDPersistence(ctx, c, marker)
		marker = uuid.NewString()
		suite.writeRAIDMarkers(ctx, c, marker)

		// Rejoin by restoring the same backing disk, not a fresh replacement.
		suite.coldBootRAID(ctx, c, []string{survivor, absent, raidDataA, raidDataB, raidDecoy})
		boot = suite.waitRAID(ctx, c, "boot", []string{raidBootA, raidBootB})
		suite.Require().Equal(suite.bootUUID, boot.uuid)
		suite.assertRAIDPersistence(ctx, c, marker)
	}
	// Read the latest degraded-write marker from the last rejoined member
	// alone. Reading a healthy mirror could otherwise be served by its peer.
	suite.coldBootRAID(ctx, c, []string{raidBootB, raidDataA, raidDataB, raidDecoy})
	suite.waitRAID(ctx, c, "boot", []string{raidBootB})
	suite.assertRAIDPersistence(ctx, c, marker)

	// Put the data and decoy devices first: neither may become the system disk.
	suite.coldBootRAID(ctx, c, []string{raidDataB, raidDecoy, raidBootB, raidDataA, raidBootA})
	boot = suite.waitRAID(ctx, c, "boot", []string{raidBootA, raidBootB})
	suite.Require().Equal(suite.bootUUID, boot.uuid)
	suite.assertRAIDPersistence(ctx, c, marker)

	// Repeat degraded writes/rejoin for the metadata-1.2 data mirror. This
	// filesystem exists only in the disposable fixture, not production config.
	for _, absent := range []string{raidDataA, raidDataB} {
		survivor := raidDataA
		if absent == raidDataA {
			survivor = raidDataB
		}
		suite.T().Logf("cold boot with data member %s physically absent", absent)
		suite.coldBootRAID(ctx, c, []string{raidBootA, raidBootB, survivor, raidDecoy})
		suite.waitRAID(ctx, c, "boot", []string{raidBootA, raidBootB})
		suite.assertRAIDPersistenceWithData(ctx, c, marker, []string{survivor})
		marker = uuid.NewString()
		suite.writeRAIDMarkersWithData(ctx, c, marker, []string{survivor})
		suite.coldBootRAID(ctx, c, []string{raidBootA, raidBootB, survivor, absent, raidDecoy})
		suite.assertRAIDPersistence(ctx, c, marker)
	}
	// Read the final recovered data peer alone, then restore/reorder all disks.
	suite.coldBootRAID(ctx, c, []string{raidBootA, raidBootB, raidDataB, raidDecoy})
	suite.assertRAIDPersistenceWithData(ctx, c, marker, []string{raidDataB})
	suite.coldBootRAID(ctx, c, []string{raidDataB, raidDecoy, raidBootB, raidDataA, raidBootA})
	suite.waitRAID(ctx, c, "boot", []string{raidBootA, raidBootB})
	suite.assertRAIDPersistence(ctx, c, marker)
}

type raidObservation struct{ device, uuid string }

func readRAIDText(ctx context.Context, c *client.Client, path string) (string, error) {
	r, err := c.Read(ctx, path)
	if err != nil {
		return "", err
	}
	defer r.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(r, 1024*1024))
	return strings.TrimSpace(string(data)), err
}

func observeRAID(ctx context.Context, c *client.Client, name string, expectedSerials []string) (raidObservation, error) {
	disks, err := safe.StateListAll[*block.Disk](ctx, c.COSI)
	if err != nil {
		return raidObservation{}, err
	}
	byID := map[string]*block.Disk{}
	var array *block.Disk
	for disk := range disks.All() {
		byID[disk.Metadata().ID()] = disk
		if slices.Contains(disk.TypedSpec().Symlinks, md.DevicePath(name)) {
			array = disk
		}
	}
	if array == nil {
		return raidObservation{}, fmt.Errorf("array %s not discovered", name)
	}
	var serials []string
	for _, member := range array.TypedSpec().SecondaryDisks {
		disk := byID[member]
		if disk == nil {
			return raidObservation{}, fmt.Errorf("array member %s not discovered", member)
		}
		serials = append(serials, disk.TypedSpec().Serial)
		memberState, err := readRAIDText(ctx, c, "/sys/block/"+array.Metadata().ID()+"/md/dev-"+member+"/state")
		if err != nil {
			return raidObservation{}, err
		}
		if !slices.Contains(strings.Split(memberState, ","), "in_sync") {
			return raidObservation{}, fmt.Errorf("member %s not in_sync: %s", member, memberState)
		}
	}
	slices.Sort(serials)
	expected := slices.Clone(expectedSerials)
	slices.Sort(expected)
	if !slices.Equal(serials, expected) {
		return raidObservation{}, fmt.Errorf("%s serial members %v, wanted %v", name, serials, expected)
	}
	metadata := "1.0"
	if name == "data" {
		metadata = "1.2"
	}
	for key, want := range map[string]string{
		"level": "raid1", "raid_disks": "2", "metadata_version": metadata,
		"degraded": strconv.Itoa(2 - len(expectedSerials)), "sync_action": "idle",
	} {
		actual, err := readRAIDText(ctx, c, "/sys/block/"+array.Metadata().ID()+"/md/"+key)
		if err != nil {
			return raidObservation{}, err
		}
		if actual != want {
			return raidObservation{}, fmt.Errorf("%s %s=%q, wanted %q", name, key, actual, want)
		}
	}
	observation := raidObservation{device: array.TypedSpec().DevPath}
	status, err := safe.StateGetByID[*storageres.MDArrayStatus](ctx, c.COSI, name)
	if err != nil {
		return observation, err
	}
	observation.uuid = status.TypedSpec().UUID
	if len(expectedSerials) == 2 {
		if status.TypedSpec().Status != storageres.MDArrayPhaseReady || status.TypedSpec().UUID == "" || status.TypedSpec().RaidDevices != 2 {
			return observation, fmt.Errorf("%s has not reached observed two-member Ready", name)
		}
	}
	if name == "boot" {
		system, err := safe.StateGetByID[*block.SystemDisk](ctx, c.COSI, block.SystemDiskID)
		if err != nil {
			return observation, err
		}
		if system.TypedSpec().DevPath != observation.device {
			return observation, fmt.Errorf("system disk is %s, not boot mirror %s", system.TypedSpec().DevPath, observation.device)
		}
	}
	return observation, nil
}

func (suite *RAIDBootSuite) waitRAID(ctx context.Context, c *client.Client, name string, serials []string) raidObservation {
	var result raidObservation
	suite.Require().EventuallyWithT(func(collect *assert.CollectT) {
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var err error
		result, err = observeRAID(attempt, c, name, serials)
		assert.NoError(collect, err)
	}, 15*time.Minute, time.Second, "waiting for exact RAID membership and completed resync")
	if result.uuid == "" {
		suite.T().Logf("%s is degraded: controller does not publish an observed UUID with one selected member; identity is checked again after rejoin", name)
	} else {
		baseline := suite.bootUUID
		if name == "data" {
			baseline = suite.dataUUID
		}
		if baseline != "" {
			suite.Require().Equal(baseline, result.uuid)
		}
	}
	return result
}

func (suite *RAIDBootSuite) coldBootRAID(ctx context.Context, c *client.Client, serials []string) {
	oldBoot, err := safe.StateGetByID[*runtimeres.BootID](ctx, c.COSI, runtimeres.BootIDID)
	suite.Require().NoError(err)
	generation, err := suite.diskControl.RebootNodeWithDisks(ctx, suite.Cluster, suite.node,
		provision.DiskLayout{Serials: serials, DiskBootOnly: true})
	suite.Require().NoError(err)
	var inventory provision.DiskBootState
	suite.Require().EventuallyWithT(func(collect *assert.CollectT) {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		boot, err := safe.StateGetByID[*runtimeres.BootID](attempt, c.COSI, runtimeres.BootIDID)
		if !assert.NoError(collect, err) {
			return
		}
		assert.NotEqual(collect, oldBoot.TypedSpec().BootID, boot.TypedSpec().BootID)
		actual, err := suite.diskControl.NodeDiskBootState(attempt, suite.Cluster, suite.node)
		if !assert.NoError(collect, err) {
			return
		}
		assert.Equal(collect, generation, actual.Generation)
		assert.NoError(collect, validateRAIDBootArguments(actual.Arguments, suite.disks, serials))
		inventory = actual
	}, 5*time.Minute, time.Second, "waiting for a new physical disk-only boot")
	suite.T().Logf("verified cold-boot generation=%s pid=%d invocation=%q", inventory.Generation, inventory.ProcessID, inventory.Arguments)
	suite.waitForClusterHealth()
	suite.assertRAIDSecurity(ctx, c)
	security, err := safe.StateGetByID[*runtimeres.SecurityState](ctx, c.COSI, runtimeres.SecurityStateID)
	suite.Require().NoError(err)
	suite.Require().True(security.TypedSpec().BootedWithUKI)
	entry, err := safe.StateGetByID[*runtimeres.BootedEntry](ctx, c.COSI, runtimeres.BootedEntryID)
	suite.Require().NoError(err)
	suite.Require().NotEmpty(entry.TypedSpec().BootedEntry)
	cmdline, err := readRAIDText(ctx, c, "/proc/cmdline")
	suite.Require().NoError(err)
	suite.Require().NotContains(cmdline, "talos.config=", "cold boot must use the persisted machine config")
	persistent, err := safe.StateGetByID[*configres.MachineConfig](ctx, c.COSI, configres.PersistentID)
	suite.Require().NoError(err)
	suite.Require().NotNil(persistent.Config().SecurityProfileConfig())
	suite.Require().True(persistent.Config().SecurityProfileConfig().WorkloadIsolation())
}

func (suite *RAIDBootSuite) assertRAIDSecurity(ctx context.Context, c *client.Client) {
	security, err := safe.StateGetByID[*runtimeres.SecurityState](ctx, c.COSI, runtimeres.SecurityStateID)
	suite.Require().NoError(err)
	suite.Require().Equal(runtimeres.SELinuxStateEnforcing, security.TypedSpec().SELinuxState)
	enforce, err := readRAIDText(ctx, c, "/sys/fs/selinux/enforce")
	suite.Require().NoError(err)
	suite.Require().Equal("1", enforce)
	active, err := safe.StateGetByID[*configres.MachineConfig](ctx, c.COSI, configres.ActiveID)
	suite.Require().NoError(err)
	suite.Require().NotNil(active.Config().SecurityProfileConfig())
	suite.Require().True(active.Config().SecurityProfileConfig().WorkloadIsolation())
}

func validateRAIDBootArguments(args []string, original []*provision.Disk, expected []string) error {
	return validateRAIDLaunchArguments(args, original, expected, true)
}

func validateRAIDInitialBootArguments(args []string, original []*provision.Disk) error {
	serials := make([]string, len(original))
	for i, disk := range original {
		serials[i] = disk.Serial
	}
	return validateRAIDLaunchArguments(args, original, serials, false)
}

func validateRAIDLaunchArguments(args []string, original []*provision.Disk, expected []string, diskOnly bool) error {
	var serials []string
	strict, kvm := false, false
	rawKernel, initramfs := false, false
	networks, firmware := 0, 0
	backends := map[string]bool{}
	devices := map[string]bool{}
	for i, arg := range args {
		if diskOnly && slices.Contains([]string{"-kernel", "-initrd", "-append", "-cdrom", "-option-rom"}, arg) {
			return fmt.Errorf("forbidden disk-only boot argument %s", arg)
		}
		if diskOnly && (strings.Contains(arg, "talos.config=") || strings.Contains(arg, "type=11")) {
			return errors.New("configuration reinjection during disk-only boot")
		}
		if i == 0 {
			continue
		}
		switch args[i-1] {
		case "-kernel":
			rawKernel = arg != "" && !strings.HasPrefix(arg, "-")
		case "-initrd":
			initramfs = arg != "" && !strings.HasPrefix(arg, "-")
		case "-machine":
			kvm = slices.Contains(strings.Split(arg, ","), "accel=kvm")
		case "-boot":
			strict = arg == "strict=on,reboot-timeout=5000"
		case "-drive":
			if slices.Contains(strings.Split(arg, ","), "if=pflash") {
				firmware++
				continue
			}
			id, _, _ := strings.Cut(arg, ",")
			if !strings.HasPrefix(id, "id=virtio") || backends[strings.TrimPrefix(id, "id=")] || !slices.Contains(strings.Split(arg, ","), "if=none") {
				return fmt.Errorf("unexpected boot media: %s", arg)
			}
			backends[strings.TrimPrefix(id, "id=")] = true
		case "-device":
			if strings.HasPrefix(arg, "virtio-net-pci,") {
				if diskOnly && (!strings.HasSuffix(arg, ",romfile=") || strings.Contains(arg, "bootindex=")) {
					return fmt.Errorf("network boot fallback enabled: %s", arg)
				}
				networks++
			}
			if strings.Contains(arg, "netdev=") && !strings.HasPrefix(arg, "virtio-net-pci,") {
				return fmt.Errorf("unexpected network boot device: %s", arg)
			}
			if !strings.HasPrefix(arg, "virtio-blk-pci,") {
				continue
			}
			properties := map[string]string{}
			for _, property := range strings.Split(arg, ",")[1:] {
				key, value, _ := strings.Cut(property, "=")
				if _, exists := properties[key]; exists {
					return fmt.Errorf("duplicate disk property: %s", arg)
				}
				properties[key] = value
			}
			serial := properties["serial"]
			index := slices.IndexFunc(original, func(d *provision.Disk) bool { return d.Serial == serial })
			bootIndex := ""
			if diskOnly {
				bootIndex = strconv.Itoa(len(serials) + 1)
			}
			if index < 0 || properties["id"] != fmt.Sprintf("talos-disk%d", index) || properties["drive"] != fmt.Sprintf("virtio%d", index) || devices[properties["drive"]] || properties["bootindex"] != bootIndex {
				return fmt.Errorf("unexpected disk identity or bootindex: %s", arg)
			}
			serials = append(serials, serial)
			devices[properties["drive"]] = true
		}
	}
	if (diskOnly && !strict) || (!diskOnly && (!rawKernel || !initramfs)) || !kvm || networks != 1 || firmware != 2 || len(backends) != len(devices) || !slices.Equal(serials, expected) {
		return fmt.Errorf("disk-only invocation mismatch: strict=%v kvm=%v networks=%d firmware=%d backends=%d devices=%d serials=%v expected=%v", strict, kvm, networks, firmware, len(backends), len(devices), serials, expected)
	}
	for device := range devices {
		if !backends[device] {
			return fmt.Errorf("disk %s has no matching backend", device)
		}
	}
	return nil
}

// validateRAIDPhysicalDisks requires the complete native virtio inventory, not
// just the one member which happened to match the failing array selector. MD
// devices are virtual and are not part of this physical serial inventory.
func validateRAIDPhysicalDisks(disks []*block.Disk, expected []*provision.Disk) error {
	remaining := make(map[string]*provision.Disk, len(expected))
	for _, disk := range expected {
		remaining[disk.Serial] = disk
	}
	for _, disk := range disks {
		if !strings.HasPrefix(disk.Metadata().ID(), "vd") {
			continue
		}
		spec := disk.TypedSpec()
		want := remaining[spec.Serial]
		if want == nil || spec.DevPath != "/dev/"+disk.Metadata().ID() || len(spec.SecondaryDisks) != 0 || spec.Size != want.Size || spec.Readonly || spec.CDROM {
			return fmt.Errorf("unexpected or duplicate native physical disk %s with serial %q", disk.Metadata().ID(), spec.Serial)
		}
		delete(remaining, spec.Serial)
	}
	if len(remaining) != 0 {
		return fmt.Errorf("native physical disk discovery is incomplete: %d expected serials missing", len(remaining))
	}
	return nil
}

func (suite *RAIDBootSuite) assertInitialRAIDInventory(ctx context.Context, c *client.Client) {
	var inventory provision.DiskBootState
	suite.Require().EventuallyWithT(func(collect *assert.CollectT) {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		actual, err := suite.diskControl.NodeDiskBootState(attempt, suite.Cluster, suite.node)
		if !assert.NoError(collect, err) {
			return
		}
		assert.Empty(collect, actual.Generation, "negative fixtures must observe their fresh initial launch")
		assert.NoError(collect, validateRAIDInitialBootArguments(actual.Arguments, suite.disks))
		disks, err := safe.StateListAll[*block.Disk](attempt, c.COSI)
		if assert.NoError(collect, err) {
			assert.NoError(collect, validateRAIDPhysicalDisks(slices.Collect(disks.All()), suite.disks))
		}
		inventory = actual
	}, 3*time.Minute, time.Second, "waiting for complete initial launch and guest physical disk inventories")
	suite.T().Logf("verified initial launch generation=%q pid=%d invocation=%q", inventory.Generation, inventory.ProcessID, inventory.Arguments)
}

func (suite *RAIDBootSuite) assertDataMount(ctx context.Context, c *client.Client, device string) {
	var observedUUID string
	suite.Require().EventuallyWithT(func(collect *assert.CollectT) {
		volume, err := safe.StateGetByID[*block.VolumeStatus](ctx, c.COSI, constants.UserVolumePrefix+"raid-data")
		if !assert.NoError(collect, err) {
			return
		}
		assert.Equal(collect, block.VolumePhaseReady, volume.TypedSpec().Phase)
		assert.Equal(collect, device, volume.TypedSpec().Location)
		assert.NotEmpty(collect, volume.TypedSpec().UUID)
		if suite.dataFilesystemUUID != "" {
			assert.Equal(collect, suite.dataFilesystemUUID, volume.TypedSpec().UUID)
		}
		mount, err := safe.StateGetByID[*block.MountStatus](ctx, c.COSI, constants.UserVolumePrefix+"raid-data")
		if !assert.NoError(collect, err) {
			return
		}
		assert.Equal(collect, raidDataPath, mount.TypedSpec().Target)
		assert.Equal(collect, volume.TypedSpec().MountLocation, mount.TypedSpec().Source)
		assert.False(collect, mount.TypedSpec().Detached)
		assert.False(collect, mount.TypedSpec().ReadOnly)
		observedUUID = volume.TypedSpec().UUID
	}, time.Minute, time.Second)
	if suite.dataFilesystemUUID == "" {
		suite.dataFilesystemUUID = observedUUID
	}
}

func (suite *RAIDBootSuite) writeRAIDMarkers(ctx context.Context, c *client.Client, marker string) {
	suite.writeRAIDMarkersWithData(ctx, c, marker, []string{raidDataA, raidDataB})
}

func (suite *RAIDBootSuite) writeRAIDMarkersWithData(ctx context.Context, c *client.Client, marker string, serials []string) {
	data := suite.waitRAID(ctx, c, "data", serials)
	suite.assertDataMount(ctx, c, data.device)
	suite.runRAIDDebug(ctx, c, []string{"/bin/sh", "-ec",
		`test -d /host/var/mnt/raid-data; printf '%s\n' "$1" > /host/var/raid-proof-marker; printf '%s\n' "$1" > /host/var/mnt/raid-data/raid-proof-marker; sync`,
		"raid-proof", marker})
}

func (suite *RAIDBootSuite) assertRAIDPersistence(ctx context.Context, c *client.Client, marker string) {
	suite.assertRAIDPersistenceWithData(ctx, c, marker, []string{raidDataA, raidDataB})
}

func (suite *RAIDBootSuite) assertRAIDPersistenceWithData(ctx context.Context, c *client.Client, marker string, serials []string) {
	data := suite.waitRAID(ctx, c, "data", serials)
	suite.assertDataMount(ctx, c, data.device)
	for _, path := range []string{raidSystemMarker, raidDataMarker} {
		actual, err := readRAIDText(ctx, c, path)
		suite.Require().NoError(err)
		suite.Require().Equal(marker, actual, "marker must survive on %s", path)
	}
	identity, err := safe.StateGetByID[*cluster.Identity](ctx, c.COSI, cluster.LocalIdentity)
	suite.Require().NoError(err)
	suite.Require().Equal(suite.identity, identity.TypedSpec().NodeID, "STATE identity must survive without config reinjection")
}

func (suite *RAIDBootSuite) runRAIDDebug(ctx context.Context, c *client.Client, args []string) {
	operation, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	suite.Require().NoError(runRAIDDebugImage(operation, c, suite.debugImage, args))
	suite.T().Logf("verified RAID marker image pull/debug binding=%s", suite.debugImage)
}

func validateRAIDDebugImage(image string) error {
	parsed, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return fmt.Errorf("-talos.provision.raid-debug-image requires a canonical name@sha256:digest: %w", err)
	}
	canonical, pinned := parsed.(reference.Canonical)
	_, tagged := parsed.(reference.Tagged)
	if !pinned || tagged || parsed.String() != image || canonical.Digest().Algorithm() != "sha256" {
		return errors.New("-talos.provision.raid-debug-image requires a fully qualified canonical name@sha256:digest without a tag")
	}
	return nil
}

// runRAIDDebugImage keeps the admitted image bound through both real API
// requests. ImageService returns a canonical name after pulling; accepting a
// different completion name would silently execute an unreviewed artifact.
func runRAIDDebugImage(ctx context.Context, c *client.Client, image string, args []string) error {
	if err := validateRAIDDebugImage(image); err != nil {
		return err
	}
	instance := &common.ContainerdInstance{Driver: common.ContainerDriver_CONTAINERD, Namespace: common.ContainerdNamespace_NS_SYSTEM}
	pull, err := c.ImageClient.Pull(ctx, &machineapi.ImageServicePullRequest{Containerd: instance, ImageRef: image})
	if err != nil {
		return fmt.Errorf("pull RAID debug image: %w", err)
	}
	completed := false
	for {
		message, err := pull.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("receive RAID image pull: %w", err)
		}
		if completed {
			return errors.New("unexpected RAID image pull response after completion")
		}
		if message.GetPullProgress() != nil {
			continue
		}
		if message.GetName() != image {
			return fmt.Errorf("RAID image pull returned %q, wanted admitted identity %q", message.GetName(), image)
		}
		completed = true
	}
	if !completed {
		return errors.New("RAID image pull ended without its admitted identity")
	}
	stream, err := c.DebugClient.ContainerRun(ctx)
	if err != nil {
		return fmt.Errorf("open RAID debug stream: %w", err)
	}
	if err := stream.Send(&machineapi.DebugContainerRunRequest{Request: &machineapi.DebugContainerRunRequest_Spec{
		Spec: &machineapi.DebugContainerRunRequestSpec{Containerd: instance, ImageName: image, Args: args, Profile: machineapi.DebugContainerRunRequestSpec_PROFILE_PRIVILEGED},
	}}); err != nil {
		return fmt.Errorf("send RAID debug specification: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("close RAID debug send stream: %w", err)
	}
	exited := false
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("receive RAID debug stream: %w", err)
		}
		if exit, ok := message.GetResp().(*machineapi.DebugContainerRunResponse_ExitCode); ok {
			if exited || exit.ExitCode != 0 {
				return fmt.Errorf("RAID marker debug exit is duplicate or unsuccessful: %d", exit.ExitCode)
			}
			exited = true
		}
	}
	if !exited {
		return errors.New("RAID debug stream ended without successful exit")
	}
	return nil
}

func (suite *RAIDBootSuite) assertFreshSelectorFailure(ctx context.Context, c *client.Client) {
	suite.Require().EventuallyWithT(func(collect *assert.CollectT) {
		status, err := safe.StateGetByID[*storageres.MDArrayStatus](ctx, c.COSI, "boot")
		if !assert.NoError(collect, err) {
			return
		}
		assert.Equal(collect, storageres.MDArrayPhaseWaiting, status.TypedSpec().Status)
		assert.Len(collect, status.TypedSpec().Members, 1)
		install, err := safe.StateGetByID[*runtimeres.UnattendedInstallStatus](ctx, c.COSI, runtimeres.UnattendedInstallStatusID)
		if !assert.NoError(collect, err) {
			return
		}
		assert.Equal(collect, runtimeres.UnattendedInstallPhasePending, install.TypedSpec().Phase)
		assert.Contains(collect, install.TypedSpec().Error, "no disk matched")
	}, 3*time.Minute, time.Second)

	// Observe beyond initial discovery; an API outage is failure, not evidence
	// that installation stayed safely blocked.
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		disks, err := safe.StateListAll[*block.Disk](ctx, c.COSI)
		suite.Require().NoError(err)
		suite.Require().NoError(validateRAIDPhysicalDisks(slices.Collect(disks.All()), suite.disks), "physical inventory must stay complete throughout the negative observation")
		install, err := safe.StateGetByID[*runtimeres.UnattendedInstallStatus](ctx, c.COSI, runtimeres.UnattendedInstallStatusID)
		suite.Require().NoError(err)
		suite.Require().Equal(runtimeres.UnattendedInstallPhasePending, install.TypedSpec().Phase)
		_, err = safe.StateGetByID[*block.SystemDisk](ctx, c.COSI, block.SystemDiskID)
		suite.Require().True(state.IsNotFoundError(err), "no disk may become the system disk: %v", err)
		volumes, err := safe.StateListAll[*block.DiscoveredVolume](ctx, c.COSI)
		suite.Require().NoError(err)
		for volume := range volumes.All() {
			suite.Require().NotEqual("partition", volume.TypedSpec().Type, "fresh data/decoy disks must not receive installation partitions")
		}
		select {
		case <-deadline.C:
			return
		case <-ctx.Done():
			suite.FailNow("negative fixture context ended", ctx.Err().Error())
		case <-tick.C:
		}
	}
}

func init() {
	for _, variant := range []string{"lifecycle", "wrong-serial", "missing-serial"} {
		allSuites = append(allSuites, &RAIDBootSuite{variant: variant})
	}
}
