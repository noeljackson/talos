// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block

import (
	"context"
	"fmt"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

// DiscoveredVolumesStatusController publishes DiscoveredVolumesStatus once devices
// and the startup MD assembly attempt are ready and a subsequent discovery refresh is done.
type DiscoveredVolumesStatusController struct{}

// Name implements controller.Controller interface.
func (ctrl *DiscoveredVolumesStatusController) Name() string {
	return "block.DiscoveredVolumesStatusController"
}

// Inputs implements controller.Controller interface.
func (ctl *DiscoveredVolumesStatusController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: runtime.NamespaceName,
			Type:      runtime.DevicesStatusType,
			ID:        optional.Some(runtime.DevicesID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: storage.NamespaceName,
			Type:      storage.MDStartupStatusType,
			ID:        optional.Some(storage.MDStartupID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.DiscoveryRefreshStatusType,
			ID:        optional.Some(block.RefreshID),
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *DiscoveredVolumesStatusController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: block.DiscoveredVolumesStatusType,
			Kind: controller.OutputExclusive,
		},
		{
			Type: block.DiscoveryRefreshRequestType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
//
// TODO(majabojarska): refactor to bring down cyclo
//
//nolint:gocyclo
func (ctrl *DiscoveredVolumesStatusController) Run(ctx context.Context, r controller.Runtime, _ *zap.Logger) error {
	var (
		devicesReadyObserved    bool
		discoveryRefreshRequest int
	)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		// if devices are not ready, we can't provision and locate most volumes
		devicesStatus, err := safe.ReaderGetByID[*runtime.DevicesStatus](ctx, r, runtime.DevicesID)
		if err != nil && !state.IsNotFoundError(err) {
			return fmt.Errorf("error fetching devices status: %w", err)
		}

		devicesReady := devicesStatus != nil && devicesStatus.TypedSpec().Ready

		mdStartupStatus, err := safe.ReaderGetByID[*storage.MDStartupStatus](ctx, r, storage.MDStartupID)
		if err != nil && !state.IsNotFoundError(err) {
			return fmt.Errorf("error fetching MD startup status: %w", err)
		}

		// Settled udev alone does not finish degraded MD assembly: the last-resort
		// owner may still be in its grace period. Do not make an absent STATE
		// definitive until that bounded startup attempt has completed. Completion
		// includes failed/not-applicable attempts and is not an array health claim.
		devicesReady = devicesReady && mdStartupStatus != nil && mdStartupStatus.TypedSpec().Complete

		if devicesReady && !devicesReadyObserved {
			devicesReadyObserved = true

			// Request a fresh scan only after both startup owners have completed;
			// an acknowledgement from before MD startup completion is insufficient.
			if err = safe.WriterModify(ctx, r, block.NewDiscoveryRefreshRequest(block.NamespaceName, block.RefreshID), func(drr *block.DiscoveryRefreshRequest) error {
				drr.TypedSpec().Request++
				discoveryRefreshRequest = drr.TypedSpec().Request

				return nil
			}); err != nil {
				return fmt.Errorf("error updating discovery refresh request: %w", err)
			}
		}

		discoveryRefreshStatus, err := safe.ReaderGetByID[*block.DiscoveryRefreshStatus](ctx, r, block.RefreshID)
		if err != nil && !state.IsNotFoundError(err) {
			return fmt.Errorf("error fetching discovery refresh status: %w", err)
		}

		// now devicesReady is only true if the refresh status is up to date
		discoveredVolumesReady := devicesReady && discoveryRefreshStatus != nil && discoveryRefreshStatus.TypedSpec().Request == discoveryRefreshRequest

		if discoveredVolumesReady {
			if err = safe.WriterModify(ctx, r, block.NewDiscoveredVolumesStatus(block.NamespaceName, block.DiscoveredVolumesStatusID), func(dvr *block.DiscoveredVolumesStatus) error {
				dvr.TypedSpec().Ready = true

				return nil
			}); err != nil {
				return fmt.Errorf("error updating discovered volumes status: %w", err)
			}
		}
	}
}
