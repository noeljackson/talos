// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage

import (
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/resource/typed"
)

// MDStartupStatusType is the type of MDStartupStatus resource.
const MDStartupStatusType = resource.Type("MDStartupStatuses.storage.talos.dev")

// MDStartupID identifies the boot-local startup assembly attempt.
const MDStartupID resource.ID = "startup"

// MDStartupStatus records completion of the startup assembly attempt, not array health.
type MDStartupStatus = typed.Resource[MDStartupStatusSpec, MDStartupStatusExtension]

// MDStartupStatusSpec survives controller restarts, but is never persisted across boots.
//
//gotagsrewrite:gen
type MDStartupStatusSpec struct {
	// Complete includes no arrays, not applicable, failure and timeout outcomes.
	Complete bool `yaml:"complete" protobuf:"1"`
	// GraceDeadline is an absolute CLOCK_BOOTTIME deadline in nanoseconds, not wall time.
	GraceDeadline int64 `yaml:"graceDeadline" protobuf:"2"`
	// AttemptDeadline bounds the shared startup attempt, without renewal on restart.
	AttemptDeadline int64 `yaml:"attemptDeadline" protobuf:"3"`
	// Attempted is recorded before running mdadm; a restarted owner never repeats it.
	Attempted bool `yaml:"attempted" protobuf:"4"`
}

// NewMDStartupStatus initializes an MDStartupStatus resource.
func NewMDStartupStatus(namespace resource.Namespace, id resource.ID) *MDStartupStatus {
	return typed.NewResource[MDStartupStatusSpec, MDStartupStatusExtension](
		resource.NewMetadata(namespace, MDStartupStatusType, id, resource.VersionUndefined),
		MDStartupStatusSpec{},
	)
}

// MDStartupStatusExtension is auxiliary resource data for MDStartupStatus.
type MDStartupStatusExtension struct{}

// ResourceDefinition implements meta.ResourceDefinitionProvider.
func (MDStartupStatusExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{
		Type: MDStartupStatusType, DefaultNamespace: NamespaceName,
		PrintColumns: []meta.PrintColumn{{Name: "Complete", JSONPath: "{.complete}"}},
	}
}

func init() {
	if err := protobuf.RegisterDynamic(MDStartupStatusType, &MDStartupStatus{}); err != nil {
		panic(err)
	}
}
