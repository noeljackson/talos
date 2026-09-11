// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package remote_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/pkg/provision"
	"github.com/siderolabs/talos/pkg/provision/providers/remote"
)

func TestDiskLayoutControlIsNotSilentlyDropped(t *testing.T) {
	t.Parallel()
	_, err := remote.MarshalClusterRequest(provision.ClusterRequest{
		Nodes: []provision.NodeRequest{{Name: "node", QEMUDiskLayoutControl: true}},
	})
	require.ErrorContains(t, err, "local native provisioner")

	_, err = remote.UnmarshalClusterRequest([]byte(`{"nodes":[{"name":"node","qemu_disk_layout_control":true}]}`))
	require.ErrorContains(t, err, "local native provisioner")
}
