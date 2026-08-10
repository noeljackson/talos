// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package services

import (
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// GetOCIOptions gets all OCI options from an Extension.
func (svc *Extension) GetOCIOptions() ([]oci.SpecOpts, error) {
	envVars, err := svc.parseEnvironment()
	if err != nil {
		return nil, err
	}

	return svc.getOCIOptions(envVars, svc.Spec.Container.Mounts), nil
}

// EnsureExtensionRootfsMountpoints exposes the extension rootfs preparation
// contract to external tests.
func EnsureExtensionRootfsMountpoints(rootfsPath string, mounts []specs.Mount) error {
	return ensureExtensionRootfsMountpoints(rootfsPath, mounts)
}
