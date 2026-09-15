// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	"golang.org/x/sys/unix"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	storagectrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/storage"
	machineruntime "github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
)

type startupBackend struct {
	mu              sync.Mutex
	inactive        []string
	listErr, runErr error
	block           bool
	calls           int
}

func (backend *startupBackend) InactiveArrays() ([]string, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return append([]string(nil), backend.inactive...), backend.listErr
}

func (backend *startupBackend) RunArray(ctx context.Context, _ string) error {
	backend.mu.Lock()
	backend.calls++
	block, err := backend.block, backend.runErr
	backend.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (backend *startupBackend) callCount() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.calls
}

type MDStartupSuite struct {
	ctest.DefaultSuite
	md   *startupBackend
	mode machineruntime.Mode
}

func (suite *MDStartupSuite) SetupTest() {
	suite.md = &startupBackend{}
	suite.DefaultSuite.AfterSetup = func(base *ctest.DefaultSuite) {
		base.Require().NoError(base.Runtime().RegisterController(&storagectrl.MDLastResortController{
			V1Alpha1Mode: suite.mode, MD: suite.md, GracePeriod: 100 * time.Millisecond, StartupAttemptTimeout: 100 * time.Millisecond,
		}))
	}
	suite.DefaultSuite.SetupTest()
}

func (suite *MDStartupSuite) healthyUdev() {
	service := v1alpha1.NewService("udevd")
	service.TypedSpec().Running = true
	service.TypedSpec().Healthy = true
	suite.Create(service)
}

func (suite *MDStartupSuite) complete() {
	ctest.AssertResource(suite, storage.MDStartupID, func(status *storage.MDStartupStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Complete)
	})
}

func (suite *MDStartupSuite) TestNoArraysCompletesImmediately() {
	suite.healthyUdev()
	suite.complete()
	ctest.AssertResource(suite, storage.MDStartupID, func(status *storage.MDStartupStatus, asrt *assert.Assertions) {
		asrt.False(status.TypedSpec().Attempted)
		asrt.Zero(status.TypedSpec().GraceDeadline)
		asrt.Zero(status.TypedSpec().AttemptDeadline)
	})
	suite.Equal(0, suite.md.callCount())
}

func (suite *MDStartupSuite) TestStartupWaitsForSettledUdev() {
	service := v1alpha1.NewService("udevd")
	service.TypedSpec().Running = true
	suite.Create(service)
	suite.Never(func() bool {
		_, err := safe.StateGetByID[*storage.MDStartupStatus](suite.Ctx(), suite.State(), storage.MDStartupID)
		return err == nil
	}, 200*time.Millisecond, 10*time.Millisecond)
	service.TypedSpec().Healthy = true
	suite.Update(service)
	suite.complete()
}

func (suite *MDStartupSuite) TestGracePreservedThenAttemptCompletes() {
	suite.md.inactive = []string{"/dev/md0"}
	suite.healthyUdev()
	ctest.AssertResource(suite, storage.MDStartupID, func(status *storage.MDStartupStatus, asrt *assert.Assertions) {
		asrt.False(status.TypedSpec().Complete)
		asrt.False(status.TypedSpec().Attempted)
		asrt.Positive(status.TypedSpec().GraceDeadline)
		asrt.Equal(int64(100*time.Millisecond), status.TypedSpec().AttemptDeadline-status.TypedSpec().GraceDeadline)
	})
	suite.Equal(0, suite.md.callCount())
	suite.complete()
	suite.Equal(1, suite.md.callCount())
}

func (suite *MDStartupSuite) TestListingFailureCompletes() {
	suite.md.listErr = errors.New("synthetic list failure")
	suite.healthyUdev()
	suite.complete()
	suite.Equal(0, suite.md.callCount())
}

func (suite *MDStartupSuite) TestAttemptFailureCompletes() {
	suite.md.inactive = []string{"/dev/md0"}
	suite.md.runErr = errors.New("synthetic run failure")
	suite.healthyUdev()
	suite.complete()
	suite.Equal(1, suite.md.callCount())
}

func (suite *MDStartupSuite) TestOneSharedAttemptBudget() {
	suite.md.inactive = []string{"/dev/md0", "/dev/md1"}
	suite.md.block = true
	suite.healthyUdev()
	suite.complete()
	// The first command exhausts the common budget. The second must not start.
	suite.Equal(1, suite.md.callCount())
	ctest.AssertResource(suite, storage.MDStartupID, func(status *storage.MDStartupStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Attempted)
	})
}

func (suite *MDStartupSuite) seedAttempt(grace, deadline time.Duration, attempted bool) (int64, int64) {
	var now unix.Timespec
	suite.Require().NoError(unix.ClockGettime(unix.CLOCK_BOOTTIME, &now))
	status := storage.NewMDStartupStatus(storage.NamespaceName, storage.MDStartupID)
	status.TypedSpec().GraceDeadline = now.Nano() + int64(grace)
	status.TypedSpec().AttemptDeadline = now.Nano() + int64(deadline)
	status.TypedSpec().Attempted = attempted
	suite.Create(status, state.WithCreateOwner("storage.MDLastResortController"))
	return status.TypedSpec().GraceDeadline, status.TypedSpec().AttemptDeadline
}

func (suite *MDStartupSuite) TestRestartResumesExistingGrace() {
	grace, deadline := suite.seedAttempt(40*time.Millisecond, 120*time.Millisecond, false)
	suite.md.inactive = []string{"/dev/md0"}
	suite.healthyUdev()
	suite.complete()
	suite.Equal(1, suite.md.callCount())
	ctest.AssertResource(suite, storage.MDStartupID, func(status *storage.MDStartupStatus, asrt *assert.Assertions) {
		asrt.Equal(grace, status.TypedSpec().GraceDeadline)
		asrt.Equal(deadline, status.TypedSpec().AttemptDeadline)
	})
}

func (suite *MDStartupSuite) TestRestartDoesNotRepeatAttempt() {
	suite.seedAttempt(-time.Second, time.Second, true)
	suite.md.inactive = []string{"/dev/md0"}
	suite.healthyUdev()
	suite.complete()
	suite.Equal(0, suite.md.callCount())
}

func (suite *MDStartupSuite) TestRestartDoesNotRenewExpiredDeadline() {
	suite.seedAttempt(-2*time.Second, -time.Second, false)
	suite.md.inactive = []string{"/dev/md0"}
	suite.healthyUdev()
	suite.complete()
	suite.Equal(0, suite.md.callCount())
}

func (suite *MDStartupSuite) TestRepeatedEventsDoNotRenewGrace() {
	suite.md.inactive = []string{"/dev/md0"}
	suite.healthyUdev()
	var grace, deadline int64
	ctest.AssertResource(suite, storage.MDStartupID, func(status *storage.MDStartupStatus, asrt *assert.Assertions) {
		asrt.Positive(status.TypedSpec().GraceDeadline)
		grace, deadline = status.TypedSpec().GraceDeadline, status.TypedSpec().AttemptDeadline
	})
	for _, name := range []string{"synthetic-a", "synthetic-b", "synthetic-c"} {
		suite.Create(block.NewDevice(block.NamespaceName, name))
	}
	suite.complete()
	ctest.AssertResource(suite, storage.MDStartupID, func(status *storage.MDStartupStatus, asrt *assert.Assertions) {
		asrt.Equal(grace, status.TypedSpec().GraceDeadline)
		asrt.Equal(deadline, status.TypedSpec().AttemptDeadline)
	})
}

func (suite *MDStartupSuite) TestRecoveryContinuesAfterStartup() {
	suite.healthyUdev()
	suite.complete()
	suite.md.mu.Lock()
	suite.md.inactive = []string{"/dev/md0"}
	suite.md.mu.Unlock()
	suite.Create(block.NewDevice(block.NamespaceName, "synthetic-late-md"))
	suite.Eventually(func() bool { return suite.md.callCount() > 0 }, time.Second, 10*time.Millisecond)
}

func TestMDStartupSuite(t *testing.T) {
	suite.Run(t, &MDStartupSuite{DefaultSuite: ctest.DefaultSuite{Timeout: 3 * time.Second}, mode: machineruntime.ModeMetal})
}

func TestMDStartupNotApplicable(t *testing.T) {
	for _, mode := range []machineruntime.Mode{machineruntime.ModeContainer, machineruntime.ModeMetalAgent} {
		t.Run(mode.String(), func(t *testing.T) {
			base := &ctest.DefaultSuite{Timeout: time.Second}
			base.SetT(t)
			base.AfterSetup = func(base *ctest.DefaultSuite) {
				// A nil backend proves these modes never access MD or wait for udev.
				base.Require().NoError(base.Runtime().RegisterController(&storagectrl.MDLastResortController{V1Alpha1Mode: mode}))
			}
			base.SetupTest()
			t.Cleanup(base.TearDownTest)
			ctest.AssertResource(base, storage.MDStartupID, func(status *storage.MDStartupStatus, asrt *assert.Assertions) {
				asrt.True(status.TypedSpec().Complete)
				asrt.False(status.TypedSpec().Attempted)
			})
		})
	}
}
