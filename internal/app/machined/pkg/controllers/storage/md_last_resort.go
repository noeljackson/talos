// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	machineruntime "github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	storageres "github.com/siderolabs/talos/pkg/machinery/resources/storage"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
)

const (
	mdLastResortGracePeriod = 30 * time.Second
	mdStartupAttemptTimeout = 10 * time.Second
)

// MDLastResortBackend lists and force-runs inactive MD arrays.
type MDLastResortBackend interface {
	InactiveArrays() ([]string, error)
	RunArray(ctx context.Context, device string) error
}

// MDLastResortController force-starts degraded MD arrays left inactive by udev.
type MDLastResortController struct {
	V1Alpha1Mode machineruntime.Mode
	GracePeriod  time.Duration
	MD           MDLastResortBackend
	// StartupAttemptTimeout bounds the single startup attempt across all arrays.
	StartupAttemptTimeout time.Duration
}

// Name implements controller.Controller.
func (ctrl *MDLastResortController) Name() string {
	return "storage.MDLastResortController"
}

// Inputs implements controller.Controller.
func (ctrl *MDLastResortController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: block.NamespaceName,
			Type:      block.DeviceType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: v1alpha1.NamespaceName,
			Type:      v1alpha1.ServiceType,
			ID:        optional.Some("udevd"),
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller.
func (ctrl *MDLastResortController) Outputs() []controller.Output {
	return []controller.Output{{Type: storageres.MDStartupStatusType, Kind: controller.OutputExclusive}}
}

func (ctrl *MDLastResortController) udevdReady(ctx context.Context, r controller.Reader, logger *zap.Logger) (bool, error) {
	udevdService, err := safe.ReaderGetByID[*v1alpha1.Service](ctx, r, "udevd")
	if err != nil && !state.IsNotFoundError(err) {
		return false, fmt.Errorf("failed to get udevd service: %w", err)
	}

	if udevdService == nil || !udevdService.TypedSpec().Running || !udevdService.TypedSpec().Healthy {
		logger.Debug("waiting for udevd service to be running and healthy")

		return false, nil
	}

	return true, nil
}

// Run implements controller.Controller.
func (ctrl *MDLastResortController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	if ctrl.V1Alpha1Mode.IsAgent() || ctrl.V1Alpha1Mode == machineruntime.ModeContainer {
		return ctrl.completeStartup(ctx, r)
	}
	if err := ctrl.awaitStartup(ctx, r, logger); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return ctrl.recoverAfterStartup(ctx, r, logger)
}

func (ctrl *MDLastResortController) awaitStartup(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	for {
		complete, remaining, err := ctrl.startup(ctx, r, logger)
		if err != nil || complete {
			return err
		}
		var timer *time.Timer
		var timerCh <-chan time.Time
		if remaining > 0 {
			timer = time.NewTimer(remaining)
			timerCh = timer.C
		}
		select {
		case <-ctx.Done():
		case <-r.EventCh():
		case <-timerCh:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// Startup completion does not disable recovery for later device events.
func (ctrl *MDLastResortController) recoverAfterStartup(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	grace := ctrl.gracePeriod()
	graceCh, err := ctrl.handleEvent(ctx, r, logger, grace, nil)
	if err != nil {
		return err
	}

	for {
		var fired bool

		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		case <-graceCh:
			graceCh = nil
			fired = true
		}

		if fired {
			if err := ctrl.forceRunInactive(ctx, logger); err != nil {
				logger.Warn("failed to force-run degraded MD arrays", zap.Error(err))
			}

			continue
		}

		graceCh, err = ctrl.handleEvent(ctx, r, logger, grace, graceCh)
		if err != nil {
			return err
		}
	}
}

func (ctrl *MDLastResortController) completeStartup(ctx context.Context, r controller.Runtime) error {
	return safe.WriterModify(ctx, r, storageres.NewMDStartupStatus(storageres.NamespaceName, storageres.MDStartupID), func(status *storageres.MDStartupStatus) error {
		status.TypedSpec().Complete = true
		return nil
	})
}

// startup gates definitive volume discovery, not array health. Its boot-clock
// deadlines survive controller restarts and cannot be extended by clock sync.
// InactiveArrays uses synchronous sysfs reads; the command budget bounds the
// cancellable mdadm attempt, not arbitrary kernel I/O or descendant processes.
func (ctrl *MDLastResortController) startup(ctx context.Context, r controller.Runtime, logger *zap.Logger) (bool, time.Duration, error) {
	status, err := safe.ReaderGetByID[*storageres.MDStartupStatus](ctx, r, storageres.MDStartupID)
	if err != nil && !state.IsNotFoundError(err) {
		return false, 0, err
	}
	if status != nil && status.TypedSpec().Complete {
		return true, 0, nil
	}
	if status != nil && status.TypedSpec().Attempted {
		// The previous owner began the only startup attempt but did not publish
		// its outcome. Do not run it again or renew its budget after a restart.
		logger.Warn("interrupted MD startup attempt; completing discovery barrier")
		return true, 0, ctrl.completeStartup(ctx, r)
	}
	if status == nil || status.TypedSpec().GraceDeadline == 0 {
		return ctrl.beginStartup(ctx, r, logger)
	}
	return ctrl.resumeStartup(ctx, r, logger, status.TypedSpec())
}

func (ctrl *MDLastResortController) beginStartup(ctx context.Context, r controller.Runtime, logger *zap.Logger) (bool, time.Duration, error) {
	ready, err := ctrl.udevdReady(ctx, r, logger)
	if err != nil || !ready {
		return false, 0, err
	}
	inactive, err := ctrl.MD.InactiveArrays()
	if err != nil {
		logger.Warn("failed to list MD arrays during startup", zap.Error(err))
		return true, 0, ctrl.completeStartup(ctx, r)
	}
	if len(inactive) == 0 {
		return true, 0, ctrl.completeStartup(ctx, r)
	}
	now, err := mdBootTime()
	if err != nil {
		return false, 0, err
	}
	budget := ctrl.StartupAttemptTimeout
	if budget <= 0 {
		budget = mdStartupAttemptTimeout
	}
	grace := ctrl.gracePeriod()
	err = safe.WriterModify(ctx, r, storageres.NewMDStartupStatus(storageres.NamespaceName, storageres.MDStartupID), func(status *storageres.MDStartupStatus) error {
		status.TypedSpec().GraceDeadline = int64(now + grace)
		status.TypedSpec().AttemptDeadline = int64(now + grace + budget)
		return nil
	})
	logger.Info("inactive MD arrays detected; will force-run degraded after grace if still stopped", zap.Strings("arrays", inactive), zap.Duration("grace", grace))
	return false, grace, err
}

func (ctrl *MDLastResortController) resumeStartup(ctx context.Context, r controller.Runtime, logger *zap.Logger, status *storageres.MDStartupStatusSpec) (bool, time.Duration, error) {
	now, err := mdBootTime()
	if err != nil {
		return false, 0, err
	}
	if remaining := time.Duration(status.GraceDeadline) - now; remaining > 0 {
		return false, remaining, nil
	}
	remaining := time.Duration(status.AttemptDeadline) - now
	if remaining <= 0 {
		logger.Warn("MD startup attempt deadline expired; completing discovery barrier")
		return true, 0, ctrl.completeStartup(ctx, r)
	}
	if err = safe.WriterModify(ctx, r, storageres.NewMDStartupStatus(storageres.NamespaceName, storageres.MDStartupID), func(status *storageres.MDStartupStatus) error {
		status.TypedSpec().Attempted = true
		return nil
	}); err != nil {
		return false, 0, err
	}
	attemptCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	if err = ctrl.forceRunInactive(attemptCtx, logger); err != nil {
		logger.Warn("failed to force-run degraded MD arrays during startup", zap.Error(err))
	}
	if ctx.Err() != nil {
		return false, 0, ctx.Err()
	}
	return true, 0, ctrl.completeStartup(ctx, r)
}

func mdBootTime() (time.Duration, error) {
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &now); err != nil {
		return 0, fmt.Errorf("failed to read boot clock: %w", err)
	}
	return time.Duration(now.Nano()), nil
}

func (ctrl *MDLastResortController) gracePeriod() time.Duration {
	if ctrl.GracePeriod != 0 {
		return ctrl.GracePeriod
	}

	return mdLastResortGracePeriod
}

func (ctrl *MDLastResortController) handleEvent(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	grace time.Duration,
	graceCh <-chan time.Time,
) (<-chan time.Time, error) {
	ready, err := ctrl.udevdReady(ctx, r, logger)
	if err != nil {
		return graceCh, err
	}

	if !ready {
		return graceCh, nil
	}

	return ctrl.armGraceIfInactive(logger, grace, graceCh), nil
}

func (ctrl *MDLastResortController) armGraceIfInactive(logger *zap.Logger, grace time.Duration, graceCh <-chan time.Time) <-chan time.Time {
	if graceCh != nil {
		return graceCh
	}

	inactive, err := ctrl.MD.InactiveArrays()
	if err != nil {
		logger.Warn("failed to list MD arrays", zap.Error(err))

		return nil
	}

	if len(inactive) == 0 {
		return nil
	}

	logger.Info("inactive MD arrays detected; will force-run degraded after grace if still stopped", zap.Strings("arrays", inactive), zap.Duration("grace", grace))

	return time.After(grace)
}

func (ctrl *MDLastResortController) forceRunInactive(ctx context.Context, logger *zap.Logger) error {
	inactive, err := ctrl.MD.InactiveArrays()
	if err != nil {
		return fmt.Errorf("failed to list MD arrays: %w", err)
	}

	var multiErr error

	for _, dev := range inactive {
		if err := ctx.Err(); err != nil {
			return errors.Join(multiErr, err)
		}
		logger.Info("force-running degraded MD array", zap.String("device", dev))

		if err := ctrl.MD.RunArray(ctx, dev); err != nil {
			multiErr = errors.Join(multiErr, fmt.Errorf("force-run %s: %w", dev, err))
		}
	}

	return multiErr
}
