// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package extensions //nolint:testpackage // test the final-composition helper directly

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	internalextensions "github.com/siderolabs/talos/internal/pkg/extensions"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	extensionsapi "github.com/siderolabs/talos/pkg/machinery/extensions"
	extservices "github.com/siderolabs/talos/pkg/machinery/extensions/services"
	"github.com/siderolabs/talos/pkg/machinery/imager/quirks"
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
	require.NoError(t, os.MkdirAll(filepath.Join(rootfsPath, "var/run/tailscale"), 0o755))
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
	require.NoError(t, os.MkdirAll(filepath.Join(rootfsPath, "var/lib/escaped"), 0o755))
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

	assert.Contains(t, policy, "(allow extension_service_p bin_exec_t (file (entrypoint execute execute_no_trans)))")
	assert.Contains(t, policy, "(allow extension_service_p lib_t (file (execute)))")
	assert.NotContains(t, policy, "(allow system_container_p bin_exec_t (file (entrypoint execute execute_no_trans)))")
	assert.NotContains(t, policy, "(allow system_container_p lib_t (file (execute)))")
	assert.Contains(t, policy, "(allow extension_service_p init_t (fd (use)))")
	assert.Contains(t, policy, "(allow extension_service_p ephemeral_t (fs_classes (rw)))")
	assert.Contains(t, policy, "(allow extension_service_p run_t (fs_classes (rw)))")
}

func TestInitramfsOverlayCredentialCanCheckImmutableExecutables(t *testing.T) {
	policyPath := filepath.Join("..", "..", "..", "internal", "pkg", "selinux", "policy", "selinux", "common")
	contents, err := os.ReadFile(filepath.Join(policyPath, "typeattributes.cil"))
	require.NoError(t, err)
	assert.Contains(t, string(contents), "(typeattributeset overlay_mounter_p initramfs_t)")

	contents, err = os.ReadFile(filepath.Join(policyPath, "processes.cil"))
	require.NoError(t, err)

	assert.Contains(t, string(contents), "(allow overlay_mounter_p any_f (file (execute)))")
}

func TestApplySystemExtensionSELinuxLabelsPreservesHostRunnerContexts(t *testing.T) {
	rootfsPath := t.TempDir()
	configPath := filepath.Join(rootfsPath, "usr/local/etc/containers/host.yaml")
	entrypointPath := filepath.Join(rootfsPath, "usr/local/bin/host-service")
	hookPath := filepath.Join(rootfsPath, "usr/bin/init")
	for _, path := range []string{configPath, entrypointPath, hookPath} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("test"), 0o755))
	}
	require.NoError(t, os.WriteFile(configPath, []byte(`name: host
runnerMode: host
restart: always
container:
  entrypoint: /usr/local/bin/host-service
preShutdown:
  entrypoint: /usr/bin/init
  timeout: 5s
`), 0o644))

	ext := &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "host", extensionsapi.Manifest{})}
	builder := &Builder{}
	require.NoError(t, builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext}))
	assert.NoDirExists(t, filepath.Join(rootfsPath, "usr/local/lib/containers/host"))
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[entrypointPath])
	assert.Equal(t, "system_u:object_r:init_exec_t:s0", builder.XAttrsMap[hookPath])
}

func TestApplySystemExtensionSELinuxLabelsRejectsInvalidHostRunner(t *testing.T) {
	rootfsPath := t.TempDir()
	configPath := filepath.Join(rootfsPath, "usr/local/etc/containers/host.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(`name: host
runnerMode: host
container:
  entrypoint: /usr/local/bin/host-service
  security:
    writeableSysfs: true
`), 0o644))
	ext := &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "host", extensionsapi.Manifest{})}
	builder := &Builder{}
	err := builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext})
	require.ErrorContains(t, err, "container security options are not supported in host runner mode")
	assert.NoDirExists(t, filepath.Join(rootfsPath, "usr/local/lib/containers/host"))
}

func TestApplySystemExtensionSELinuxLabelsDefersBaseFileMountpoints(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "absent destination"
		if existing {
			name = "existing file destination"
		}

		t.Run(name, func(t *testing.T) {
			ext, serviceRoot := newServiceExtension(t, extservices.Container{
				Entrypoint: "/usr/local/bin/service",
				Mounts:     []specs.Mount{{Source: "/etc/os-release", Destination: "/etc/os-release", Type: "bind"}},
			})
			writeServiceTestFile(t, filepath.Join(serviceRoot, "usr/local/bin/service"), "executable", 0o755)
			destination := filepath.Join(serviceRoot, "etc/os-release")
			if existing {
				writeServiceTestFile(t, destination, "retained placeholder", 0o600)
			}

			builder := &Builder{}
			require.NoError(t, builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext}))
			if !existing {
				_, err := os.Lstat(destination)
				require.ErrorIs(t, err, fs.ErrNotExist)
				assert.NotContains(t, builder.XAttrsMap, destination)

				return
			}

			contents, err := os.ReadFile(destination)
			require.NoError(t, err)
			assert.Equal(t, "retained placeholder", string(contents))
			info, err := os.Stat(destination)
			require.NoError(t, err)
			assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
			assert.Equal(t, constants.EtcSelinuxLabel, builder.XAttrsMap[destination])
		})
	}
}

func TestApplySystemExtensionSELinuxLabelsRejectsSymlinkedServiceRoots(t *testing.T) {
	for _, linkedPath := range []string{"usr/local/lib/containers", "usr/local/lib/containers/service"} {
		t.Run(linkedPath, func(t *testing.T) {
			rootfsPath := t.TempDir()
			outsidePath := t.TempDir()
			writeServiceTestFile(t, filepath.Join(rootfsPath, "usr/local/etc/containers/service.yaml"), "name: service\nrestart: always\ncontainer:\n  entrypoint: /service\n", 0o644)
			symlinkPath := filepath.Join(rootfsPath, linkedPath)
			require.NoError(t, os.MkdirAll(filepath.Dir(symlinkPath), 0o755))
			require.NoError(t, os.Symlink(outsidePath, symlinkPath))
			ext := &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "service", extensionsapi.Manifest{})}

			builder := &Builder{}
			err := builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext})
			require.ErrorContains(t, err, "error opening extension service rootfs")
			entries, err := os.ReadDir(outsidePath)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestApplySystemExtensionSELinuxLabelsPrefersExactFileBindSources(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "created placeholder"
		if existing {
			name = "shadowed rootfs executable"
		}

		t.Run(name, func(t *testing.T) {
			ext, serviceRoot := newServiceExtension(t, extservices.Container{
				Entrypoint: "/service",
				Args:       []string{"/helper"},
				Mounts: []specs.Mount{
					{Source: "/usr/local/lib/service", Destination: "/service", Type: "bind"},
					{Source: "/usr/local/lib/helper", Destination: "/helper", Type: "bind"},
				},
			})
			providerRoot := t.TempDir()
			provider := &internalextensions.Extension{Extension: extensionsapi.New(providerRoot, "provider", extensionsapi.Manifest{})}
			for _, binary := range []string{"service", "helper"} {
				writeServiceTestFile(t, filepath.Join(providerRoot, "usr/local/lib", binary), "mounted executable", 0o755)
				if existing {
					writeServiceTestFile(t, filepath.Join(serviceRoot, binary), "shadowed executable", 0o755)
				}
			}

			builder := &Builder{}
			require.NoError(t, builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext, provider}))
			for _, binary := range []string{"service", "helper"} {
				assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[filepath.Join(providerRoot, "usr/local/lib", binary)])
				assert.NotEqual(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[filepath.Join(serviceRoot, binary)])
			}
		})
	}
}

func TestApplySystemExtensionSELinuxLabelsResolvesExecutableSymlinksInRoot(t *testing.T) {
	ext, serviceRoot := newServiceExtension(t, extservices.Container{Entrypoint: "/usr/local/lib/loader", Args: []string{"/helper"}})
	loaderTarget := filepath.Join(serviceRoot, "usr/local/lib/loader-real")
	helperTarget := filepath.Join(serviceRoot, "usr/local/lib/helper-real")
	writeServiceTestFile(t, loaderTarget, "loader", 0o755)
	writeServiceTestFile(t, helperTarget, "helper", 0o755)
	require.NoError(t, os.Symlink("/usr/local/lib/loader-real", filepath.Join(serviceRoot, "usr/local/lib/loader")))
	require.NoError(t, os.Symlink("/usr/local/lib/helper-real", filepath.Join(serviceRoot, "helper")))

	builder := &Builder{}
	require.NoError(t, builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext}))
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[loaderTarget])
	assert.Equal(t, constants.SystemExtensionBinSELinuxLabel, builder.XAttrsMap[helperTarget])
}

func TestApplySystemExtensionSELinuxLabelsRejectsShadowedEntrypointWithoutBindSource(t *testing.T) {
	ext, serviceRoot := newServiceExtension(t, extservices.Container{
		Entrypoint: "/service",
		Mounts:     []specs.Mount{{Source: "/usr/local/lib/missing", Destination: "/service", Type: "bind"}},
	})
	writeServiceTestFile(t, filepath.Join(serviceRoot, "service"), "shadowed executable", 0o755)

	builder := &Builder{}
	err := builder.applySystemExtensionSELinuxLabels([]*internalextensions.Extension{ext})
	require.ErrorContains(t, err, "absent from the service rootfs and mounted extension sources")
}

func TestExtensionServiceFileInRootPreservesExtractionPath(t *testing.T) {
	actualRoot := t.TempDir()
	parentAlias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(actualRoot, parentAlias))
	root := filepath.Join(parentAlias, "rootfs")
	target := filepath.Join(root, "usr/local/lib/service")
	writeServiceTestFile(t, target, "executable", 0o755)
	require.NoError(t, os.Symlink("/usr/local/lib/service", filepath.Join(root, "service")))

	file, err := extensionServiceFileInRoot(root, "/service")
	require.NoError(t, err)
	assert.Equal(t, target, file.path)
	assert.True(t, file.info.Mode().IsRegular())
}

func TestCompressExtensionsLabelsFinalGeneratedLayers(t *testing.T) {
	suppliedRoot := t.TempDir()
	generatedRoot := t.TempDir()
	writeServiceTestFile(t, filepath.Join(suppliedRoot, "usr/local/lib/library.so"), "library", 0o644)
	generatedFile := filepath.Join(generatedRoot, "usr/lib/modules/6.18.0/modules.dep")
	writeServiceTestFile(t, generatedFile, "module dependencies", 0o644)
	extensions := []*internalextensions.Extension{
		{Extension: extensionsapi.New(suppliedRoot, "supplied", extensionsapi.Manifest{})},
		{Extension: extensionsapi.New(generatedRoot, "modules.dep", extensionsapi.Manifest{})},
	}
	builder := &Builder{Quirks: quirks.New("1.14.0"), Printf: func(string, ...any) {}}

	// Stop before invoking mksquashfs; the final layer list must already have
	// complete labels even when compression itself is cancelled.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := builder.compressExtensions(ctx, extensions, t.TempDir())
	require.Error(t, err)

	for path, label := range map[string]string{
		generatedRoot:                                   "system_u:object_r:rootfs_t:s0",
		filepath.Join(generatedRoot, "usr"):             "system_u:object_r:usr_t:s0",
		filepath.Join(generatedRoot, "usr/lib"):         constants.SystemExtensionLibSELinuxLabel,
		filepath.Join(generatedRoot, "usr/lib/modules"): "system_u:object_r:module_t:s0",
		generatedFile:                                   "system_u:object_r:module_t:s0",
	} {
		assert.Equal(t, label, builder.XAttrsMap[path], path)
	}
}

func newServiceExtension(t *testing.T, container extservices.Container) (*internalextensions.Extension, string) {
	t.Helper()
	rootfsPath := t.TempDir()
	serviceRoot := filepath.Join(rootfsPath, "usr/local/lib/containers/service")
	require.NoError(t, os.MkdirAll(serviceRoot, 0o755))
	data, err := yaml.Marshal(extservices.Spec{Name: "service", Restart: extservices.RestartAlways, Container: container})
	require.NoError(t, err)
	writeServiceTestFile(t, filepath.Join(rootfsPath, "usr/local/etc/containers/service.yaml"), string(data), 0o644)

	return &internalextensions.Extension{Extension: extensionsapi.New(rootfsPath, "service", extensionsapi.Manifest{})}, serviceRoot
}

func writeServiceTestFile(t *testing.T, path, contents string, mode fs.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), mode))
}
