// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package extensions //nolint:testpackage // test the final-composition helper directly

import (
	"io/fs"
	"os"
	"path/filepath"
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

	assert.Equal(t, "system_u:object_r:rootfs_t:s0", builder.XAttrsMap[rootfsPath])
	assert.Equal(t, "system_u:object_r:usr_t:s0", builder.XAttrsMap[filepath.Join(rootfsPath, "usr")])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[filepath.Dir(binPath)])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[binPath])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[symlinkPath])
	assert.Equal(t, constants.SystemExtensionLibSELinuxLabel, builder.XAttrsMap[libPath])
	assert.Equal(t, constants.KubeletCredentialProviderSELinuxLabel, builder.XAttrsMap[credentialProviderPath])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[containerBinPath])
	assert.Equal(t, constants.SystemExtensionLibSELinuxLabel, builder.XAttrsMap[containerLibPath])
	assert.Equal(t, constants.EtcSelinuxLabel, builder.XAttrsMap[containerEtcPath])
	assert.Equal(t, "system_u:object_r:rootfs_t:s0", builder.XAttrsMap[containerRootPath])
	assert.Equal(t, "system_u:object_r:usr_t:s0", builder.XAttrsMap[outsidePath])
}

func TestApplySystemExtensionSELinuxLabelsInitializesMap(t *testing.T) {
	rootfsPath := t.TempDir()
	binPath := filepath.Join(rootfsPath, "usr/local/bin/runtime")

	require.NoError(t, os.MkdirAll(filepath.Dir(binPath), 0o755))
	require.NoError(t, os.WriteFile(binPath, []byte("test"), 0o755))

	ext := &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "test", extensionsapi.Manifest{})}
	builder := &Builder{}

	require.NoError(t, builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext}))
	assert.Equal(t, "system_u:object_r:rootfs_t:s0", builder.XAttrsMap[rootfsPath])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[binPath])
}

func TestApplySystemExtensionSELinuxLabelsPreparesDeclaredMountpointsInContainerRoot(t *testing.T) {
	rootfsPath := t.TempDir()
	configPath := filepath.Join(rootfsPath, "usr/local/etc/containers/tailscale.yaml")
	serviceRootfsPath := filepath.Join(rootfsPath, "usr/local/lib/containers/tailscale")
	entrypointPath := filepath.Join(serviceRootfsPath, "usr/local/bin/containerboot")
	tailscaleRunPath := filepath.Join(serviceRootfsPath, "run/tailscale")

	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(entrypointPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(serviceRootfsPath, "var"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(serviceRootfsPath, "run"), 0o755))
	require.NoError(t, os.Symlink("/run", filepath.Join(serviceRootfsPath, "var/run")))
	require.NoError(t, os.WriteFile(configPath, []byte(`name: tailscale
restart: always
container:
  entrypoint: /usr/local/bin/containerboot
  mounts:
    - source: /var/run/tailscale
      destination: /var/run/tailscale
      type: bind
`), 0o644))
	require.NoError(t, os.WriteFile(entrypointPath, []byte("containerboot"), 0o755))

	ext := &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "tailscale", extensionsapi.Manifest{})}
	builder := &Builder{}

	require.NoError(t, builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext}))
	assert.DirExists(t, tailscaleRunPath)
	assert.Equal(t, constants.RunSelinuxLabel, builder.XAttrsMap[tailscaleRunPath])
	assert.Equal(t, constants.EtcSelinuxLabel, builder.XAttrsMap[filepath.Join(serviceRootfsPath, "etc/hosts")])

	info, err := os.Lstat(filepath.Join(serviceRootfsPath, "var/run"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
}

func TestApplySystemExtensionSELinuxLabelsRejectsMountpointSymlinkEscape(t *testing.T) {
	rootfsPath := t.TempDir()
	outsidePath := t.TempDir()
	configPath := filepath.Join(rootfsPath, "usr/local/etc/containers/escaped.yaml")
	serviceRootfsPath := filepath.Join(rootfsPath, "usr/local/lib/containers/escaped")
	entrypointPath := filepath.Join(serviceRootfsPath, "usr/local/bin/service")

	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(entrypointPath), 0o755))
	require.NoError(t, os.Symlink(outsidePath, filepath.Join(serviceRootfsPath, "var")))
	require.NoError(t, os.WriteFile(configPath, []byte(`name: escaped
restart: always
container:
  entrypoint: /usr/local/bin/service
  mounts:
    - source: /var/lib/escaped
      destination: /var/lib/escaped
      type: bind
`), 0o644))
	require.NoError(t, os.WriteFile(entrypointPath, []byte("service"), 0o755))

	ext := &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "escaped", extensionsapi.Manifest{})}
	builder := &Builder{}

	err := builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext})
	require.Error(t, err)
	assert.NoDirExists(t, filepath.Join(outsidePath, "lib"))
}

func TestApplySystemExtensionSELinuxLabelsUsesDeclaredServiceEntrypoint(t *testing.T) {
	rootfsPath := t.TempDir()
	configPath := filepath.Join(rootfsPath, "usr/local/etc/containers/nydus.yaml")
	serviceLibPath := filepath.Join(rootfsPath, "usr/local/lib/containers/nydus/usr/local/lib")
	serviceRootfsPath := filepath.Join(rootfsPath, "usr/local/lib/containers/nydus")
	entrypointPath := filepath.Join(serviceLibPath, "ld-linux-x86-64.so.2")
	adjacentLibraryPath := filepath.Join(serviceLibPath, "libc.so.6")
	executableArgumentPath := filepath.Join(serviceRootfsPath, "containerd-nydus-grpc")
	dataArgumentPath := filepath.Join(serviceRootfsPath, "config.toml")

	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.MkdirAll(serviceLibPath, 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(`name: nydus
restart: always
container:
  entrypoint: /usr/local/lib/ld-linux-x86-64.so.2
  args:
    - --library-path
    - /usr/local/lib
    - /containerd-nydus-grpc
    - /config.toml
`), 0o644))
	require.NoError(t, os.WriteFile(entrypointPath, []byte("loader"), 0o755))
	require.NoError(t, os.WriteFile(adjacentLibraryPath, []byte("library"), 0o755))
	require.NoError(t, os.WriteFile(executableArgumentPath, []byte("executable"), 0o755))
	require.NoError(t, os.WriteFile(dataArgumentPath, []byte("data"), 0o644))

	ext := &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "test", extensionsapi.Manifest{})}
	builder := &Builder{}

	require.NoError(t, builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext}))
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[entrypointPath])
	assert.Equal(t, constants.SystemExtensionLibSELinuxLabel, builder.XAttrsMap[adjacentLibraryPath])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[executableArgumentPath])
	assert.NotEqual(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[dataArgumentPath])
}

func TestApplySystemExtensionSELinuxLabelsUsesMountedServiceEntrypoint(t *testing.T) {
	configRootfsPath := t.TempDir()
	providerRootfsPath := t.TempDir()
	configPath := filepath.Join(configRootfsPath, "usr/local/etc/containers/iscsid.yaml")
	serviceRootfsPath := filepath.Join(configRootfsPath, "usr/local/lib/containers/iscsid")
	entrypointPath := filepath.Join(providerRootfsPath, "usr/local/lib/iscsid")
	adjacentLibraryPath := filepath.Join(providerRootfsPath, "usr/local/lib/libiscsi.so")

	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.MkdirAll(serviceRootfsPath, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(entrypointPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(`name: iscsid
restart: always
container:
  entrypoint: /usr/local/lib/iscsid
  mounts:
    - source: /usr/local/lib
      destination: /usr/local/lib
      type: bind
`), 0o644))
	require.NoError(t, os.WriteFile(entrypointPath, []byte("iscsid"), 0o755))
	require.NoError(t, os.WriteFile(adjacentLibraryPath, []byte("library"), 0o755))

	configExt := &internalextensions.Extension{Extension: extensionsapi.New(configRootfsPath, "config", extensionsapi.Manifest{})}
	providerExt := &internalextensions.Extension{Extension: extensionsapi.New(providerRootfsPath, "provider", extensionsapi.Manifest{})}
	builder := &Builder{}

	require.NoError(t, builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{configExt, providerExt}))
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[entrypointPath])
	assert.Equal(t, constants.SystemExtensionLibSELinuxLabel, builder.XAttrsMap[adjacentLibraryPath])
}

func TestApplySystemExtensionSELinuxLabelsRejectsMissingServiceEntrypoint(t *testing.T) {
	rootfsPath := t.TempDir()
	configPath := filepath.Join(rootfsPath, "usr/local/etc/containers/missing.yaml")
	serviceRootfsPath := filepath.Join(rootfsPath, "usr/local/lib/containers/missing")

	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.MkdirAll(serviceRootfsPath, 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(`name: missing
restart: always
container:
  entrypoint: /usr/local/lib/missing
  mounts:
    - source: /usr/local/lib
      destination: /usr/local/lib
      type: bind
`), 0o644))

	ext := &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "test", extensionsapi.Manifest{})}
	builder := &Builder{}

	err := builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent from the service rootfs and mounted extension sources")
}

func TestSystemExtensionSELinuxLabelUsesPathBoundariesAndSpecificity(t *testing.T) {
	testCases := map[string]struct {
		path  string
		mode  fs.FileMode
		label string
		ok    bool
	}{
		"extension root": {
			path:  "/",
			mode:  fs.ModeDir,
			label: "system_u:object_r:rootfs_t:s0",
			ok:    true,
		},
		"top-level usr": {
			path:  "/usr",
			mode:  fs.ModeDir,
			label: "system_u:object_r:usr_t:s0",
			ok:    true,
		},
		"direct path specificity": {
			path:  "/usr/local/lib/kubelet/credentialproviders/helper",
			label: constants.KubeletCredentialProviderSELinuxLabel,
			ok:    true,
		},
		"direct generic path": {
			path:  "/usr/local/binary/runtime",
			label: "system_u:object_r:usr_t:s0",
			ok:    true,
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
		"nested runtime state": {
			path:  "/usr/local/lib/containers/tailscale/run/tailscale",
			mode:  fs.ModeDir,
			label: constants.RunSelinuxLabel,
			ok:    true,
		},
		"nested persistent state": {
			path:  "/usr/local/lib/containers/tailscale/var/lib/tailscale",
			mode:  fs.ModeDir,
			label: constants.EphemeralSelinuxLabel,
			ok:    true,
		},
		"nested generic path uses container namespace": {
			path:  "/usr/local/lib/containers/tailscale/etc/tailscale/config",
			label: constants.EtcSelinuxLabel,
			ok:    true,
		},
		"nested generic executable path": {
			path:  "/usr/local/lib/containers/tailscale/usr/local/binary/containerboot",
			label: "system_u:object_r:usr_t:s0",
			ok:    true,
		},
		"container root": {
			path:  "/usr/local/lib/containers/tailscale",
			mode:  fs.ModeDir,
			label: "system_u:object_r:rootfs_t:s0",
			ok:    true,
		},
		"file-specific context": {
			path:  "/usr/bin/init",
			label: "system_u:object_r:init_exec_t:s0",
			ok:    true,
		},
		"file-specific context respects type": {
			path:  "/usr/bin/init",
			mode:  fs.ModeDir,
			label: constants.SystemExtensionBinSELinuxLabel,
			ok:    true,
		},
		"missing container name": {
			path: "/usr/local/lib/containers//usr/local/bin/containerboot",
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			label, ok, err := systemExtensionSELinuxLabel(testCase.path, testCase.mode)
			require.NoError(t, err)
			assert.Equal(t, testCase.ok, ok)
			assert.Equal(t, testCase.label, label)
		})
	}
}

func TestExtensionSystemContainerPolicyAllowsLabeledExecutablesAndLibraries(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "pkg", "selinux", "policy", "selinux", "services", "system-containerd.cil"))
	require.NoError(t, err)

	policy := string(contents)

	assert.Contains(t, policy, "(allow system_container_p bin_exec_t (file (entrypoint execute execute_no_trans)))")
	assert.Contains(t, policy, "(allow system_container_p lib_t (file (execute)))")
	assert.Contains(t, policy, "(allow unconfined_container_t init_t (fd (use)))")
	assert.Contains(t, policy, "(allow unconfined_container_t ephemeral_t (fs_classes (rw)))")
	assert.Contains(t, policy, "(allow unconfined_container_t run_t (fs_classes (rw)))")
}

func TestInitramfsOverlayCredentialCanCheckImmutableExecutables(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "pkg", "selinux", "policy", "selinux", "services", "machined.cil"))
	require.NoError(t, err)

	assert.Contains(t, string(contents), "(allow initramfs_t system_f (file (execute)))")
}
