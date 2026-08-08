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
		"usr/local/lib/containers/tailscale/usr/local/bin",
		"usr/local/lib/containers/tailscale/usr/lib",
		"usr/local/lib/containers/tailscale/etc/tailscale",
		"usr/local/share",
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(rootfsPath, path), 0o755))
	}

	binPath := filepath.Join(rootfsPath, "usr/local/bin/runtime")
	libPath := filepath.Join(rootfsPath, "usr/local/lib/runtime.so")
	credentialProviderPath := filepath.Join(rootfsPath, "usr/local/lib/kubelet/credentialproviders/helper")
	containerBinPath := filepath.Join(rootfsPath, "usr/local/lib/containers/tailscale/usr/local/bin/containerboot")
	containerLibPath := filepath.Join(rootfsPath, "usr/local/lib/containers/tailscale/usr/lib/libtailscale.so")
	containerEtcPath := filepath.Join(rootfsPath, "usr/local/lib/containers/tailscale/etc/tailscale/config")
	containerRootPath := filepath.Join(rootfsPath, "usr/local/lib/containers/tailscale")
	outsidePath := filepath.Join(rootfsPath, "usr/local/share/data")
	symlinkPath := filepath.Join(rootfsPath, "usr/local/bin/runtime-link")

	for _, path := range []string{binPath, libPath, credentialProviderPath, containerBinPath, containerLibPath, containerEtcPath, outsidePath} {
		require.NoError(t, os.WriteFile(path, []byte("test"), 0o755))
	}

	require.NoError(t, os.Symlink("runtime", symlinkPath))

	ext := &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "test", extensionsapi.Manifest{})}
	builder := &Builder{XAttrsMap: map[string]string{
		binPath:                "artifact_u:object_r:artifact_t:s0",
		credentialProviderPath: "artifact_u:object_r:artifact_t:s0",
		containerEtcPath:       "artifact_u:object_r:artifact_t:s0",
		outsidePath:            "artifact_u:object_r:artifact_t:s0",
	}}

	require.NoError(t, builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext}))

	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[filepath.Dir(binPath)])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[binPath])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[symlinkPath])
	assert.Equal(t, constants.SystemExtensionLibSELinuxLabel, builder.XAttrsMap[libPath])
	assert.Equal(t, constants.KubeletCredentialProviderSELinuxLabel, builder.XAttrsMap[credentialProviderPath])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[containerBinPath])
	assert.Equal(t, constants.SystemExtensionLibSELinuxLabel, builder.XAttrsMap[containerLibPath])
	assert.Equal(t, "artifact_u:object_r:artifact_t:s0", builder.XAttrsMap[containerEtcPath])
	assert.NotContains(t, builder.XAttrsMap, containerRootPath)
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
	testCases := map[string]struct {
		path  string
		label string
		ok    bool
	}{
		"direct path specificity": {
			path:  "/usr/local/lib/kubelet/credentialproviders/helper",
			label: constants.KubeletCredentialProviderSELinuxLabel,
			ok:    true,
		},
		"direct path boundary": {
			path: "/usr/local/binary/runtime",
		},
		"nested executable": {
			path:  "/usr/local/lib/containers/tailscale/usr/local/bin/containerboot",
			label: constants.SystemExtensionBinSELinuxLabel,
			ok:    true,
		},
		"nested library": {
			path:  "/usr/local/lib/containers/tailscale/usr/lib/libtailscale.so",
			label: constants.SystemExtensionLibSELinuxLabel,
			ok:    true,
		},
		"nested path does not inherit outer library label": {
			path: "/usr/local/lib/containers/tailscale/etc/tailscale/config",
		},
		"nested executable path boundary": {
			path: "/usr/local/lib/containers/tailscale/usr/local/binary/containerboot",
		},
		"container root": {
			path: "/usr/local/lib/containers/tailscale",
		},
		"missing container name": {
			path: "/usr/local/lib/containers//usr/local/bin/containerboot",
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			label, ok := systemExtensionSELinuxLabel(testCase.path)
			assert.Equal(t, testCase.ok, ok)
			assert.Equal(t, testCase.label, label)
		})
	}
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

func TestExtensionSystemContainerPolicyAllowsLabeledExecutablesAndLibraries(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "pkg", "selinux", "policy", "selinux", "services", "system-containerd.cil"))
	require.NoError(t, err)

	policy := string(contents)

	assert.Contains(t, policy, "(allow system_container_p bin_exec_t (file (entrypoint execute execute_no_trans)))")
	assert.Contains(t, policy, "(allow system_container_p lib_t (file (execute)))")
}
