// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package extensions //nolint:testpackage // test the final-composition helper directly

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	internalextensions "github.com/siderolabs/talos/internal/pkg/extensions"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	extensionsapi "github.com/siderolabs/talos/pkg/machinery/extensions"
)

func TestApplySystemExtensionSELinuxLabels(t *testing.T) {
	rootfsPath := t.TempDir()

	for _, path := range []string{
		"usr/local/bin",
		"usr/local/lib",
		"usr/local/lib/kubelet/credentialproviders",
		"usr/local/share",
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(rootfsPath, path), 0o755))
	}

	binPath := filepath.Join(rootfsPath, "usr/local/bin/runtime")
	libPath := filepath.Join(rootfsPath, "usr/local/lib/runtime.so")
	credentialProviderPath := filepath.Join(rootfsPath, "usr/local/lib/kubelet/credentialproviders/helper")
	outsidePath := filepath.Join(rootfsPath, "usr/local/share/data")
	symlinkPath := filepath.Join(rootfsPath, "usr/local/bin/runtime-link")

	for _, path := range []string{binPath, libPath, credentialProviderPath, outsidePath} {
		require.NoError(t, os.WriteFile(path, []byte("test"), 0o755))
	}

	require.NoError(t, os.Symlink("runtime", symlinkPath))

	ext := &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "test", extensionsapi.Manifest{})}
	builder := &Builder{XAttrsMap: map[string]string{
		binPath:                "artifact_u:object_r:artifact_t:s0",
		credentialProviderPath: "artifact_u:object_r:artifact_t:s0",
		outsidePath:            "artifact_u:object_r:artifact_t:s0",
	}}

	require.NoError(t, builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext}))

	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[filepath.Dir(binPath)])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[binPath])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[symlinkPath])
	assert.Equal(t, constants.SystemExtensionLibSELinuxLabel, builder.XAttrsMap[libPath])
	assert.Equal(t, constants.KubeletCredentialProviderSELinuxLabel, builder.XAttrsMap[credentialProviderPath])
	assert.Equal(t, "artifact_u:object_r:artifact_t:s0", builder.XAttrsMap[outsidePath])
}

func TestApplySystemExtensionSELinuxLabelsInitializesMap(t *testing.T) {
	rootfsPath := t.TempDir()
	binPath := filepath.Join(rootfsPath, "usr/local/bin/runtime")

	require.NoError(t, os.MkdirAll(filepath.Dir(binPath), 0o755))
	require.NoError(t, os.WriteFile(binPath, []byte("test"), 0o755))

	ext := &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "test", extensionsapi.Manifest{})}
	builder := &Builder{}

	require.NoError(t, builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext}))
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[binPath])
}

func TestSystemExtensionSELinuxLabelUsesPathBoundariesAndSpecificity(t *testing.T) {
	label, ok := systemExtensionSELinuxLabel("/usr/local/lib/kubelet/credentialproviders/helper")
	assert.True(t, ok)
	assert.Equal(t, constants.KubeletCredentialProviderSELinuxLabel, label)

	_, ok = systemExtensionSELinuxLabel("/usr/local/binary/runtime")
	assert.False(t, ok)
}

func TestSystemExtensionSELinuxLabelsMatchFileContexts(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "pkg", "selinux", "policy", "file_contexts"))
	require.NoError(t, err)

	fileContexts := string(contents)

	for _, labeledPath := range constants.SystemExtensionSELinuxLabeledPaths {
		assert.Contains(t, fileContexts, labeledPath.Path+"(/.*)?\t"+labeledPath.Label)
	}

	assert.Equal(t, 1, strings.Count(fileContexts, constants.KubeletCredentialProviderBinDir+"(/.*)?\t"+constants.KubeletCredentialProviderSELinuxLabel))
}
