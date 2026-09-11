// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage_test

import (
	"path/filepath"

	"github.com/stretchr/testify/assert"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	"github.com/siderolabs/talos/internal/pkg/md"
	storageres "github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

const (
	bootSerialSelector = `disk.serial == "BOOT-A" || disk.serial == "BOOT-B"`
	dataSerialSelector = `disk.serial == "DATA-A" || disk.serial == "DATA-B"`
	// Use nonexistent device names so even a privileged test run cannot trigger
	// a change event on an actual host MD array.
	bootTestDevice = "/dev/md-test-boot"
	dataTestDevice = "/dev/md-test-data"
)

type serialDiskFixture struct {
	device string
	serial string
}

func (suite *MDArrayReconcileSuite) createSerialDisks(disks ...serialDiskFixture) {
	for _, disk := range disks {
		createDiskWithSerial(&suite.DefaultSuite, filepath.Base(disk.device), disk.device, "virtio", disk.serial)
	}
}

func (suite *MDArrayReconcileSuite) createSerialMirrorSpecs() {
	for _, array := range []struct {
		name     string
		selector string
		metadata storageres.MDMetadata
	}{
		{"boot", bootSerialSelector, storageres.MDMetadata10},
		{"data", dataSerialSelector, storageres.MDMetadata12},
	} {
		spec := storageres.NewMDArraySpec(storageres.NamespaceName, array.name)
		spec.TypedSpec().Level = storageres.MDLevelRAID1
		spec.TypedSpec().Metadata = array.metadata
		suite.Require().NoError(spec.TypedSpec().VolumeSelector.UnmarshalText([]byte(array.selector)))
		suite.Create(spec)
	}
}

// observeSerialMirror changes only the simulated mdadm/sysfs observation. The
// production controller still decides whether to create, add, or grow an array.
func (suite *MDArrayReconcileSuite) observeSerialMirror(name, device string, members []string, action md.SyncAction) {
	suite.md.mu.Lock()
	defer suite.md.mu.Unlock()

	metadata := "1.0"
	if name == "data" {
		metadata = "1.2"
	}

	suite.md.details[device] = md.Detail{
		Level:       "raid1",
		RaidDevices: 2,
		Members:     append([]string(nil), members...),
		Name:        "talos:" + name,
		UUID:        "test-" + name,
		Metadata:    metadata,
	}
	suite.md.syncAction[device] = action

	for member, arrayDevice := range suite.md.findByMember {
		if arrayDevice == device {
			delete(suite.md.findByMember, member)
		}
	}

	for _, member := range members {
		suite.md.findByMember[member] = device
	}
}

func (suite *MDArrayReconcileSuite) assertSerialMirror(name string, members []string, phase storageres.MDArrayPhase, action md.SyncAction) {
	ctest.AssertResource(suite, name, func(status *storageres.MDArrayStatus, asrt *assert.Assertions) {
		spec := status.TypedSpec()
		asrt.Equal(storageres.MDLevelRAID1, spec.Level)
		asrt.Equal(phase, spec.Status)
		asrt.ElementsMatch(members, spec.Members)
		asrt.Equal(2, spec.RaidDevices)
		asrt.Equal(md.DevicePath(name), spec.Device)
		asrt.Equal("talos:"+name, spec.Name)
		asrt.Equal("test-"+name, spec.UUID)
		asrt.Equal(string(action), spec.SyncAction)
		asrt.Empty(spec.Error)

		if name == "boot" {
			asrt.Equal("1.0", spec.Metadata)
		} else {
			asrt.Equal("1.2", spec.Metadata)
		}
	})
}

// assertNoSerialMirrorMutations checks every array, not just the expected name,
// so an unintended mutation of the other mirror or a decoy cannot pass.
func (suite *MDArrayReconcileSuite) assertNoSerialMirrorMutations() {
	suite.md.mu.Lock()
	defer suite.md.mu.Unlock()

	suite.Assert().Empty(suite.md.creates)
	suite.Assert().Empty(suite.md.adds)
	suite.Assert().Empty(suite.md.grows)
}

func (suite *MDArrayReconcileSuite) TestCreatesIndependentMirrorsByExactSerial() {
	suite.md.createNodes["boot"] = bootTestDevice
	suite.md.createNodes["data"] = dataTestDevice

	// All disks have the same transport. Discovery order, device order, serial
	// prefixes, and an absent serial must not broaden either exact selector.
	suite.createSerialDisks(
		serialDiskFixture{"/dev/vdf", "DATA-A"},
		serialDiskFixture{"/dev/vdb", "BOOT-B"},
		serialDiskFixture{"/dev/vdg", ""},
		serialDiskFixture{"/dev/vda", "BOOT-A-extra"},
		serialDiskFixture{"/dev/vdd", "BOOT-A"},
		serialDiskFixture{"/dev/vde", "data-a"},
		serialDiskFixture{"/dev/vdc", "DATA-B"},
	)
	suite.createSerialMirrorSpecs()

	suite.assertSerialMirror("boot", []string{"/dev/vdb", "/dev/vdd"}, storageres.MDArrayPhaseReady, md.SyncActionIdle)
	suite.assertSerialMirror("data", []string{"/dev/vdc", "/dev/vdf"}, storageres.MDArrayPhaseReady, md.SyncActionIdle)

	suite.md.mu.Lock()
	defer suite.md.mu.Unlock()

	suite.Assert().Equal(map[string]md.CreateOptions{
		"boot": {Level: 1, Metadata: "1.0", RaidDevices: 2, Devices: []string{"/dev/vdb", "/dev/vdd"}},
		"data": {Level: 1, Metadata: "1.2", RaidDevices: 2, Devices: []string{"/dev/vdc", "/dev/vdf"}},
	}, suite.md.creates)
	suite.Assert().Empty(suite.md.adds)
	suite.Assert().Empty(suite.md.grows)
}

func (suite *MDArrayReconcileSuite) TestSerialMirrorsIgnoreRenamedReorderedDevices() {
	// Model rediscovery after a cold boot: serials and MD identities persist,
	// but member paths and discovery order differ from the initial-create test.
	// This controller test does not boot a VM or simulate firmware behavior.
	suite.observeSerialMirror("boot", bootTestDevice, []string{"/dev/vdh", "/dev/vdc"}, md.SyncActionIdle)
	suite.observeSerialMirror("data", dataTestDevice, []string{"/dev/vdg", "/dev/vdb"}, md.SyncActionIdle)
	suite.createSerialDisks(
		serialDiskFixture{"/dev/vdh", "BOOT-B"},
		serialDiskFixture{"/dev/vdg", "DATA-B"},
		serialDiskFixture{"/dev/vdf", ""},
		serialDiskFixture{"/dev/vde", "DATA-A-extra"},
		serialDiskFixture{"/dev/vdc", "BOOT-A"},
		serialDiskFixture{"/dev/vdb", "DATA-A"},
		serialDiskFixture{"/dev/vda", "boot-a"},
	)
	suite.createSerialMirrorSpecs()

	suite.assertSerialMirror("boot", []string{"/dev/vdc", "/dev/vdh"}, storageres.MDArrayPhaseReady, md.SyncActionIdle)
	suite.assertSerialMirror("data", []string{"/dev/vdb", "/dev/vdg"}, storageres.MDArrayPhaseReady, md.SyncActionIdle)
	suite.assertNoSerialMirrorMutations()
}

func (suite *MDArrayReconcileSuite) assertIncompleteBootSerialSelection(secondSerial string, absent, existing bool) {
	// A healthy data mirror and prefix-matching decoy must never satisfy the
	// incomplete system selector, even when a system array already exists.
	suite.observeSerialMirror("data", dataTestDevice, []string{"/dev/vdc", "/dev/vdd"}, md.SyncActionIdle)
	if existing {
		suite.observeSerialMirror("boot", bootTestDevice, []string{"/dev/vda"}, md.SyncActionIdle)
	}

	suite.createSerialDisks(
		serialDiskFixture{"/dev/vda", "BOOT-A"},
		serialDiskFixture{"/dev/vdc", "DATA-A"},
		serialDiskFixture{"/dev/vdd", "DATA-B"},
		serialDiskFixture{"/dev/vde", "BOOT-B-extra"},
	)
	if !absent {
		suite.createSerialDisks(serialDiskFixture{"/dev/vdb", secondSerial})
	}

	suite.createSerialMirrorSpecs()

	ctest.AssertResource(suite, "boot", func(status *storageres.MDArrayStatus, asrt *assert.Assertions) {
		asrt.Equal(storageres.MDArrayPhaseWaiting, status.TypedSpec().Status)
		asrt.Equal([]string{"/dev/vda"}, status.TypedSpec().Members)
		asrt.Equal("waiting for enough member disks: matched 1, required 2", status.TypedSpec().Error)
	})
	suite.assertSerialMirror("data", []string{"/dev/vdc", "/dev/vdd"}, storageres.MDArrayPhaseReady, md.SyncActionIdle)
	suite.assertNoSerialMirrorMutations()
}

func (suite *MDArrayReconcileSuite) TestWrongSerialDoesNotCreate() {
	suite.assertIncompleteBootSerialSelection("WRONG", false, false)
}

func (suite *MDArrayReconcileSuite) TestMissingSerialDoesNotCreate() {
	suite.assertIncompleteBootSerialSelection("", false, false)
}

func (suite *MDArrayReconcileSuite) TestAbsentSerialMemberDoesNotCreate() {
	suite.assertIncompleteBootSerialSelection("", true, false)
}

func (suite *MDArrayReconcileSuite) TestWrongSerialDoesNotGrowExistingArray() {
	suite.assertIncompleteBootSerialSelection("WRONG", false, true)
}

func (suite *MDArrayReconcileSuite) TestMissingSerialDoesNotGrowExistingArray() {
	suite.assertIncompleteBootSerialSelection("", false, true)
}

func (suite *MDArrayReconcileSuite) TestAbsentSerialMemberDoesNotGrowExistingArray() {
	suite.assertIncompleteBootSerialSelection("", true, true)
}

func (suite *MDArrayReconcileSuite) TestTwoMemberSerialMirrorRejoinsWithoutGrowing() {
	suite.observeSerialMirror("boot", bootTestDevice, []string{"/dev/vda"}, md.SyncActionIdle)
	suite.observeSerialMirror("data", dataTestDevice, []string{"/dev/vdc", "/dev/vdd"}, md.SyncActionIdle)
	suite.createSerialDisks(
		serialDiskFixture{"/dev/vda", "BOOT-A"},
		serialDiskFixture{"/dev/vdb", "BOOT-B-extra"},
		serialDiskFixture{"/dev/vdc", "DATA-A"},
		serialDiskFixture{"/dev/vdd", "DATA-B"},
	)
	suite.createSerialMirrorSpecs()

	ctest.AssertResource(suite, "boot", func(status *storageres.MDArrayStatus, asrt *assert.Assertions) {
		asrt.Equal(storageres.MDArrayPhaseWaiting, status.TypedSpec().Status)
		asrt.Equal([]string{"/dev/vda"}, status.TypedSpec().Members)
	})
	suite.assertSerialMirror("data", []string{"/dev/vdc", "/dev/vdd"}, storageres.MDArrayPhaseReady, md.SyncActionIdle)
	suite.assertNoSerialMirrorMutations()

	// The returning member has a different device name; the old pathname now
	// belongs to a decoy. Only its exact serial may authorize the add operation.
	suite.createSerialDisks(serialDiskFixture{"/dev/vdf", "BOOT-B"})
	suite.eventually(func() bool {
		return len(suite.md.added(bootTestDevice)) > 0
	})

	// These are explicit fixture observations, not evidence of real resync.
	members := []string{"/dev/vda", "/dev/vdf"}
	suite.observeSerialMirror("boot", bootTestDevice, members, md.SyncActionRecover)
	refresh := storageres.NewMDRefreshRequest(storageres.NamespaceName, storageres.RefreshID)
	refresh.TypedSpec().Request = 1
	suite.Create(refresh)
	suite.assertSerialMirror("boot", members, storageres.MDArrayPhaseRebuilding, md.SyncActionRecover)

	suite.observeSerialMirror("boot", bootTestDevice, members, md.SyncActionIdle)
	ctest.UpdateWithConflicts(suite, refresh, func(request *storageres.MDRefreshRequest) error {
		request.TypedSpec().Request++

		return nil
	})
	suite.assertSerialMirror("boot", members, storageres.MDArrayPhaseReady, md.SyncActionIdle)
	suite.assertSerialMirror("data", []string{"/dev/vdc", "/dev/vdd"}, storageres.MDArrayPhaseReady, md.SyncActionIdle)

	suite.md.mu.Lock()
	defer suite.md.mu.Unlock()

	suite.Assert().Empty(suite.md.creates, "rejoin must retain the original arrays")
	suite.Assert().Equal(map[string]map[string]struct{}{
		bootTestDevice: {"/dev/vdf": {}},
	}, suite.md.adds)
	suite.Assert().Empty(suite.md.grows, "rejoining a missing slot must retain two configured RAID devices")
}
