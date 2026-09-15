// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block_test

import (
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	blockctrls "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/block"
	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

type DiscoveredVolumesStatusSuite struct {
	ctest.DefaultSuite
}

func TestDiscoveredVolumesStatusSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, &DiscoveredVolumesStatusSuite{
		DefaultSuite: ctest.DefaultSuite{
			Timeout: 5 * time.Second,
			AfterSetup: func(suite *ctest.DefaultSuite) {
				suite.Require().NoError(suite.Runtime().RegisterController(&blockctrls.DiscoveredVolumesStatusController{}))
			},
		},
	})
}

func (suite *DiscoveredVolumesStatusSuite) TestNoDevicesStatus() {
	suite.completeMDStartup()
	ctest.AssertNoResource[*block.DiscoveredVolumesStatus](suite, block.DiscoveredVolumesStatusID)
	ctest.AssertNoResource[*block.DiscoveryRefreshRequest](suite, block.RefreshID)
}

func (suite *DiscoveredVolumesStatusSuite) TestDevicesNotReady() {
	suite.completeMDStartup()
	devicesStatus := runtime.NewDevicesStatus(runtime.NamespaceName, runtime.DevicesID)
	devicesStatus.TypedSpec().Ready = false
	suite.Create(devicesStatus)

	ctest.AssertNoResource[*block.DiscoveryRefreshRequest](suite, block.RefreshID)
	ctest.AssertNoResource[*block.DiscoveredVolumesStatus](suite, block.DiscoveredVolumesStatusID)
}

func (suite *DiscoveredVolumesStatusSuite) TestDevicesReadyRequestsRefreshButNotYetDone() {
	suite.completeMDStartup()
	devicesStatus := runtime.NewDevicesStatus(runtime.NamespaceName, runtime.DevicesID)
	devicesStatus.TypedSpec().Ready = true
	suite.Create(devicesStatus)

	ctest.AssertResource(suite, block.RefreshID, func(r *block.DiscoveryRefreshRequest, asrt *assert.Assertions) {
		asrt.Equal(1, r.TypedSpec().Request)
	})

	ctest.AssertNoResource[*block.DiscoveredVolumesStatus](suite, block.DiscoveredVolumesStatusID)
}

func (suite *DiscoveredVolumesStatusSuite) TestDiscoveryRefreshStatusMatchMakesReady() {
	suite.completeMDStartup()
	// Setup: create devices ready and observe the refresh request bump.
	devicesStatus := runtime.NewDevicesStatus(runtime.NamespaceName, runtime.DevicesID)
	devicesStatus.TypedSpec().Ready = true
	suite.Create(devicesStatus)

	ctest.AssertResource(suite, block.RefreshID, func(r *block.DiscoveryRefreshRequest, asrt *assert.Assertions) {
		asrt.Equal(1, r.TypedSpec().Request)
	})

	// Regression test for the missing DiscoveryRefreshStatus input — before the fix, this write alone
	// would never wake the controller, and AssertResource would timeout.
	refreshStatus := block.NewDiscoveryRefreshStatus(block.NamespaceName, block.RefreshID)
	refreshStatus.TypedSpec().Request = 1
	suite.Create(refreshStatus)

	ctest.AssertResource(suite, block.DiscoveredVolumesStatusID, func(r *block.DiscoveredVolumesStatus, asrt *assert.Assertions) {
		asrt.True(r.TypedSpec().Ready)
	})
}

func (suite *DiscoveredVolumesStatusSuite) TestStaleDiscoveryRefreshStatusDoesNotMarkReady() {
	suite.completeMDStartup()
	// Setup: create devices ready and observe the refresh request bump.
	devicesStatus := runtime.NewDevicesStatus(runtime.NamespaceName, runtime.DevicesID)
	devicesStatus.TypedSpec().Ready = true
	suite.Create(devicesStatus)

	ctest.AssertResource(suite, block.RefreshID, func(r *block.DiscoveryRefreshRequest, asrt *assert.Assertions) {
		asrt.Equal(1, r.TypedSpec().Request)
	})

	// Create a stale/mismatched discovery refresh status.
	refreshStatus := block.NewDiscoveryRefreshStatus(block.NamespaceName, block.RefreshID)
	refreshStatus.TypedSpec().Request = 0
	suite.Create(refreshStatus)

	// Give the controller real wall-clock time to react to this write and confirm it
	// correctly declines to mark ready when the status doesn't match.
	ctx, st := suite.Ctx(), suite.State()
	suite.Assert().Never(func() bool {
		_, err := safe.StateGetByID[*block.DiscoveredVolumesStatus](ctx, st, block.DiscoveredVolumesStatusID)

		return err == nil
	}, 1*time.Second, 100*time.Millisecond)
}

func (suite *DiscoveredVolumesStatusSuite) TestReadyDoesNotResetWhenDevicesBecomeNotReady() {
	suite.completeMDStartup()
	// Drive the full happy path to get DiscoveredVolumesStatus.Ready = true.
	devicesStatus := runtime.NewDevicesStatus(runtime.NamespaceName, runtime.DevicesID)
	devicesStatus.TypedSpec().Ready = true
	suite.Create(devicesStatus)

	ctest.AssertResource(suite, block.RefreshID, func(r *block.DiscoveryRefreshRequest, asrt *assert.Assertions) {
		asrt.Equal(1, r.TypedSpec().Request)
	})

	refreshStatus := block.NewDiscoveryRefreshStatus(block.NamespaceName, block.RefreshID)
	refreshStatus.TypedSpec().Request = 1
	suite.Create(refreshStatus)

	ctest.AssertResource(suite, block.DiscoveredVolumesStatusID, func(r *block.DiscoveredVolumesStatus, asrt *assert.Assertions) {
		asrt.True(r.TypedSpec().Ready)
	})

	// Flip devices back to not ready.
	devicesStatus.TypedSpec().Ready = false
	suite.Require().NoError(suite.State().Update(suite.Ctx(), devicesStatus))

	// Confirm Ready remains true (intentional one-way latch behavior).
	ctest.AssertResource(suite, block.DiscoveredVolumesStatusID, func(r *block.DiscoveredVolumesStatus, asrt *assert.Assertions) {
		asrt.True(r.TypedSpec().Ready)
	})
}

func (suite *DiscoveredVolumesStatusSuite) completeMDStartup() {
	status := storage.NewMDStartupStatus(storage.NamespaceName, storage.MDStartupID)
	status.TypedSpec().Complete = true
	suite.Create(status)
}

func (suite *DiscoveredVolumesStatusSuite) assertNoRefreshWhileMDPending() {
	ctx, st := suite.Ctx(), suite.State()
	suite.Assert().Never(func() bool {
		_, refreshErr := safe.StateGetByID[*block.DiscoveryRefreshRequest](ctx, st, block.RefreshID)
		_, readyErr := safe.StateGetByID[*block.DiscoveredVolumesStatus](ctx, st, block.DiscoveredVolumesStatusID)
		return refreshErr == nil || readyErr == nil
	}, 200*time.Millisecond, 10*time.Millisecond)
}

func (suite *DiscoveredVolumesStatusSuite) TestMissingMDStartupBlocksDefinitiveDiscovery() {
	devicesStatus := runtime.NewDevicesStatus(runtime.NamespaceName, runtime.DevicesID)
	devicesStatus.TypedSpec().Ready = true
	suite.Create(devicesStatus)
	suite.assertNoRefreshWhileMDPending()
}

func (suite *DiscoveredVolumesStatusSuite) TestMDStartupCompletionRequiresFreshRefresh() {
	startup := storage.NewMDStartupStatus(storage.NamespaceName, storage.MDStartupID)
	suite.Create(startup)
	devicesStatus := runtime.NewDevicesStatus(runtime.NamespaceName, runtime.DevicesID)
	devicesStatus.TypedSpec().Ready = true
	suite.Create(devicesStatus)
	// A scan completed before MD startup cannot make missing volumes definitive.
	refreshStatus := block.NewDiscoveryRefreshStatus(block.NamespaceName, block.RefreshID)
	suite.Create(refreshStatus)
	suite.assertNoRefreshWhileMDPending()

	startup.TypedSpec().Complete = true
	suite.Update(startup)
	ctest.AssertResource(suite, block.RefreshID, func(request *block.DiscoveryRefreshRequest, asrt *assert.Assertions) {
		asrt.Equal(1, request.TypedSpec().Request)
	})
	suite.Assert().Never(func() bool {
		_, err := safe.StateGetByID[*block.DiscoveredVolumesStatus](suite.Ctx(), suite.State(), block.DiscoveredVolumesStatusID)
		return err == nil
	}, 200*time.Millisecond, 10*time.Millisecond)
	refreshStatus.TypedSpec().Request = 1
	suite.Update(refreshStatus)
	ctest.AssertResource(suite, block.DiscoveredVolumesStatusID, func(status *block.DiscoveredVolumesStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready)
	})
}
