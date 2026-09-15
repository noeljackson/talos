// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage_test

import (
	"testing"

	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/cosi-project/runtime/pkg/state/impl/namespaced"
	"github.com/cosi-project/runtime/pkg/state/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

func TestRegisterResource(t *testing.T) {
	ctx := t.Context()

	resources := state.WrapCore(namespaced.NewState(inmem.Build))
	resourceRegistry := registry.NewResourceRegistry(resources)

	for _, resource := range []meta.ResourceWithRD{
		&storage.LVMVolumeGroupStatus{},
		&storage.LVMLogicalVolumeStatus{},
		&storage.LVMPhysicalVolumeStatus{},
		&storage.LVMRefreshRequest{},
		&storage.MDRefreshRequest{},
		&storage.MDStartupStatus{},
	} {
		assert.NoError(t, resourceRegistry.Register(ctx, resource))
	}
}

func TestMDStartupStatusRoundTrip(t *testing.T) {
	original := storage.NewMDStartupStatus(storage.NamespaceName, storage.MDStartupID)
	*original.TypedSpec() = storage.MDStartupStatusSpec{Complete: true, GraceDeadline: 123456789, AttemptDeadline: 223456789, Attempted: true}
	encoded, err := protobuf.FromResource(original, protobuf.WithoutYAML())
	require.NoError(t, err)
	wire, err := encoded.Marshal()
	require.NoError(t, err)
	decoded, err := protobuf.Unmarshal(wire)
	require.NoError(t, err)
	roundTrip, err := protobuf.UnmarshalResource(decoded)
	require.NoError(t, err)
	result, ok := roundTrip.(*storage.MDStartupStatus)
	require.True(t, ok)
	assert.Equal(t, *original.TypedSpec(), *result.TypedSpec())
	copy := original.DeepCopy().(*storage.MDStartupStatus)
	copy.TypedSpec().Complete = false
	assert.True(t, original.TypedSpec().Complete)
}
