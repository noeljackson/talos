// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package selinux

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupFileContextForPersistentOverlays(t *testing.T) {
	t.Parallel()

	testCases := map[string]string{
		"/etc/cni/net.d":                          "system_u:object_r:cni_conf_t:s0",
		"/etc/kubernetes/kubeconfig":              "system_u:object_r:k8s_conf_t:s0",
		"/usr/libexec/kubernetes/kubelet-plugins": "system_u:object_r:k8s_plugin_t:s0",
		"/opt":                          "system_u:object_r:opt_t:s0",
		"/opt/cni/bin":                  "system_u:object_r:cni_plugin_t:s0",
		"/opt/cni/bin/cilium-sysctlfix": "system_u:object_r:cni_plugin_t:s0",
		"/opt/containerd/io.containerd.snapshotter": "system_u:object_r:containerd_plugin_t:s0",
	}

	for path, expected := range testCases {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			context, ok, err := LookupFileContext(path, 0)
			require.NoError(t, err)
			require.True(t, ok, "LookupFileContext(%q) did not match", path)
			assert.Equal(t, expected, context)
		})
	}
}

func TestLookupFileContextForKataHostHelpers(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"/usr/local/bin/cloud-hypervisor",
		"/usr/local/bin/qemu-system-x86_64-snp-experimental",
		"/usr/local/libexec/qemu-system-x86_64-snp-experimental",
		"/usr/local/libexec/virtiofsd",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			context, ok, err := LookupFileContext(path, 0)
			require.NoError(t, err)
			require.True(t, ok, "LookupFileContext(%q) did not match", path)
			assert.Equal(t, "system_u:object_r:kata_helper_exec_t:s0", context)
		})
	}
}

func TestCiliumRuntimePolicyAllowsKubeletHostPathSetup(t *testing.T) {
	t.Parallel()

	policy, err := os.ReadFile("policy/selinux/services/kubelet.cil")
	require.NoError(t, err)

	assert.Contains(t, string(policy), "(allow kubelet_t cilium_runtime_t (fs_classes (rw)))")
	assert.NotContains(t, string(policy), "(allow kubelet_t pod_containerd_run_t")
}

func TestCiliumDomainMayInstallCNIBinaries(t *testing.T) {
	t.Parallel()

	policy, err := os.ReadFile("policy/selinux/services/cri.cil")
	require.NoError(t, err)

	assert.Contains(t, string(policy), "(allow cilium_t cni_plugin_t (fs_classes (rw)))")
	assert.Contains(t, string(policy), "(allow cilium_t bpf_t (fs_classes (rw)))")
	assert.Contains(t, string(policy), "(allow cilium_t self (perf_event (all)))")
	assert.Contains(t, string(policy), "(allow cilium_t containerd_p (bpf (prog_run)))")
	assert.Contains(t, string(policy), "(allow pod_containerd_t cilium_t (unix_stream_socket (connectto)))")
	assert.Contains(t, string(policy), "(allow cilium_t run_t (dir (getattr open read search)))")
	assert.Contains(t, string(policy), "(typeattributeset mcs_exempt_p cilium_t)")
}

func TestOverlayCompositorMayCompleteCNILowerExecuteCheck(t *testing.T) {
	t.Parallel()

	machinedPolicy, err := os.ReadFile("policy/selinux/services/machined.cil")
	require.NoError(t, err)
	criPolicy, err := os.ReadFile("policy/selinux/services/cri.cil")
	require.NoError(t, err)

	// OverlayFS checks the real CRI caller first, then the credential stashed by
	// initramfs when it composed /opt. Both narrowly scoped edges are required.
	assert.Contains(t, string(criPolicy), "(allow pod_containerd_t cni_plugin_t (file (execute_no_trans execute)))")
	assert.Contains(t, string(machinedPolicy), "(allow initramfs_t cni_plugin_t (file (execute)))")
	assert.NotContains(t, string(machinedPolicy), "(allow initramfs_t cni_plugin_t (fs_classes")
	assert.NotContains(t, string(machinedPolicy), "(allow initramfs_t cni_plugin_t (file (entrypoint")
	assert.NotContains(t, string(machinedPolicy), "(allow initramfs_t cni_plugin_t (file (execute_no_trans")
}

func TestCRIContainerdMayDeliverKataShimLogsToTalosSyslogd(t *testing.T) {
	t.Parallel()

	policy, err := os.ReadFile("policy/selinux/services/cri.cil")
	require.NoError(t, err)

	// Kata shim v2 initializes its required system logger before VM creation.
	// Talos syslogd runs in init_t; keep this edge to datagram delivery only.
	assert.Contains(t, string(policy), "(allow pod_containerd_t init_t (unix_dgram_socket (sendto)))")
	assert.NotContains(t, string(policy), "(allow pod_containerd_t init_t (unix_dgram_socket (all)))")
}

func TestCRIContainerdMayConnectToKataVMMSandboxSocket(t *testing.T) {
	t.Parallel()

	policy, err := os.ReadFile("policy/selinux/services/cri.cil")
	require.NoError(t, err)

	// The Kata shim supervises the sandbox from pod_containerd_t while the VMM
	// adopts the sandbox pod_p label. Keep the required API-socket edge limited
	// to peer connection; filesystem access is authorized separately.
	assert.Contains(t, string(policy), "(allow pod_containerd_t pod_p (unix_stream_socket (connectto)))")
	assert.NotContains(t, string(policy), "(allow pod_containerd_t pod_p (unix_stream_socket (all)))")
}

func TestCRIContainerdMayConnectToDefaultExtensionServices(t *testing.T) {
	t.Parallel()

	policy, err := os.ReadFile("policy/selinux/services/cri.cil")
	require.NoError(t, err)

	// Talos runs extension services such as Nydus and iSCSI in the default
	// unconfined_container_t domain. CRI clients still need the peer-domain
	// connect check after the independently labeled socket path is authorized.
	assert.Contains(t, string(policy), "(allow pod_containerd_t unconfined_container_t (unix_stream_socket (connectto)))")
	assert.NotContains(t, string(policy), "(allow pod_containerd_t system_container_p (unix_stream_socket")
	assert.NotContains(t, string(policy), "(allow pod_containerd_t unconfined_container_t (unix_stream_socket (all)))")
	assert.NotContains(t, string(policy), "(allow pod_containerd_t unconfined_container_t (fs_classes")
	assert.NotContains(t, string(policy), "(allow pod_containerd_t unconfined_container_t (process")
}

func TestCRIContainerdMayOpenItsNamedKataTAP(t *testing.T) {
	t.Parallel()

	criPolicy, err := os.ReadFile("policy/selinux/services/cri.cil")
	require.NoError(t, err)
	networkPolicy, err := os.ReadFile("policy/selinux/common/network.cil")
	require.NoError(t, err)

	// Linux's selinux_tun_dev_open hook checks both permissions while moving an
	// existing TUN security SID to the current caller, including a self-relabel.
	assert.Contains(t, string(criPolicy), "(allow pod_containerd_t self (tun_socket (relabelfrom relabelto)))")
	assert.NotContains(t, string(criPolicy), "(allow pod_p self (tun_socket")
	assert.NotContains(t, string(criPolicy), "(allow any_p any_p (tun_socket (relabelfrom")
	assert.NotContains(t, string(networkPolicy), "relabelfrom")
	assert.NotContains(t, string(networkPolicy), "relabelto")
}

func TestHostAgentDomainsHaveBoundedReadOnlyIntrospection(t *testing.T) {
	t.Parallel()

	policy, err := os.ReadFile("policy/selinux/services/cri.cil")
	require.NoError(t, err)

	// containerd v2 clears an explicit process label for privileged CRI
	// sandboxes. Those trusted host agents inherit pod_containerd_t, which may
	// inspect process metadata but receives no process or write permission.
	assert.Contains(t, string(policy), "(allow pod_containerd_t any_p (dir (getattr open read search)))")
	assert.Contains(t, string(policy), "(allow pod_containerd_t any_p (file (getattr open read)))")
	assert.Contains(t, string(policy), "(allow pod_containerd_t any_p (lnk_file (getattr read)))")
	assert.NotContains(t, string(policy), "(allow pod_containerd_t any_p (process")
	assert.NotContains(t, string(policy), "(allow pod_containerd_t any_p (fs_classes (rw)))")

	// Non-privileged node-exporter receives its own explicitly selected domain.
	// Generic pod_t must not inherit either host-process or udev access.
	assert.Contains(t, string(policy), "(type node_exporter_t)")
	assert.Contains(t, string(policy), "(call pod_p (node_exporter_t))")
	assert.Contains(t, string(policy), "(allow node_exporter_t init_t (dir (getattr open read search)))")
	assert.Contains(t, string(policy), "(allow node_exporter_t init_t (file (getattr open read)))")
	assert.Contains(t, string(policy), "(allow node_exporter_t init_t (lnk_file (getattr read)))")
	assert.Contains(t, string(policy), "(allow node_exporter_t udev_run_t (file (getattr open read)))")
	assert.NotContains(t, string(policy), "(allow pod_t udev_run_t")
	assert.NotContains(t, string(policy), "(dontaudit pod_containerd_t any_p")
	assert.NotContains(t, string(policy), "(dontaudit node_exporter_t")
}

func TestFalcoDomainHasBoundedLeastPrivilegedHostObserverAccess(t *testing.T) {
	t.Parallel()

	policy, err := os.ReadFile("policy/selinux/services/cri.cil")
	require.NoError(t, err)

	assert.Contains(t, string(policy), "(type falco_t)")
	assert.Contains(t, string(policy), "(call pod_p (falco_t))")
	assert.Contains(t, string(policy), "(typeattributeset mcs_exempt_p falco_t)")
	assert.Contains(t, string(policy), "(allow falco_t any_p (dir (getattr open read search)))")
	assert.Contains(t, string(policy), "(allow falco_t any_p (file (getattr open read)))")
	assert.Contains(t, string(policy), "(allow falco_t any_p (lnk_file (getattr read)))")
	assert.Contains(t, string(policy), "(allow falco_t any_p (unix_stream_socket (getattr)))")
	assert.Contains(t, string(policy), "(allow falco_t any_p (unix_dgram_socket (getattr)))")
	assert.Contains(t, string(policy), "(allow falco_t any_p (fifo_file (getattr)))")
	assert.Contains(t, string(policy), "(allow falco_t self (perf_event (all)))")
	assert.Contains(t, string(policy), "(allow falco_t tracefs_t (dir (getattr open read search)))")
	assert.Contains(t, string(policy), "(allow falco_t tracefs_t (file (getattr open read)))")
	assert.Contains(t, string(policy), "(allow falco_t pod_containerd_socket_t (sock_file (write)))")
	assert.Contains(t, string(policy), "(allow falco_t pod_containerd_t (unix_stream_socket (connectto)))")
	assert.NotContains(t, string(policy), "(allow falco_t any_p (process")
	assert.NotContains(t, string(policy), "(allow falco_t any_p (fs_classes (rw)))")
	assert.NotContains(t, string(policy), "(allow falco_t any_p (unix_stream_socket (all)))")
	assert.NotContains(t, string(policy), "(allow falco_t any_p (unix_dgram_socket (all)))")
	assert.NotContains(t, string(policy), "(allow falco_t tracefs_t (fs_classes (rw)))")
	assert.NotContains(t, string(policy), "(allow falco_t debugfs_t")
	assert.NotContains(t, string(policy), "(allow falco_t sys_containerd_socket_t")
	assert.NotContains(t, string(policy), "(allow falco_t sys_containerd_t")
	assert.NotContains(t, string(policy), "(dontaudit falco_t")
}

func TestKataPodDomainHasOnlyDedicatedHostHelperEntrypoints(t *testing.T) {
	t.Parallel()

	policy, err := os.ReadFile("policy/selinux/services/cri.cil")
	require.NoError(t, err)

	for _, path := range []string{
		"/usr/local/bin/cloud-hypervisor",
		"/usr/local/bin/qemu-system-x86_64-snp-experimental",
		"/usr/local/libexec/qemu-system-x86_64-snp-experimental",
		"/usr/local/libexec/virtiofsd",
	} {
		assert.Contains(t, string(policy), `(filecon "`+path+`" file kata_helper_exec_t)`)
	}

	assert.Contains(t, string(policy), "(allow pod_containerd_t kata_helper_exec_t (file (execute_no_trans execute)))")
	assert.Contains(t, string(policy), "(allow pod_t kata_helper_exec_t (file (entrypoint execute)))")
	assert.NotContains(t, string(policy), "(allow pod_p bin_exec_t (file (entrypoint")
	assert.NotContains(t, string(policy), "(allow pod_t bin_exec_t (file (entrypoint")
}

func TestKataPodDomainMayStartVirtiofsd(t *testing.T) {
	t.Parallel()

	policy, err := os.ReadFile("policy/selinux/services/cri.cil")
	require.NoError(t, err)

	// Kata launches virtiofsd and Cloud Hypervisor under the sandbox's
	// MCS-scoped pod_t label. These are the complete startup edges observed in
	// a permissive run that returned a sandbox ID, combined with the earlier
	// enforcing denials that stopped before the later checks were reached.
	assert.Contains(t, string(policy), "(allow pod_t pod_containerd_t (unix_stream_socket (read write accept connectto)))")
	assert.Contains(t, string(policy), "(allow pod_t init_t (unix_dgram_socket (sendto)))")
	assert.Contains(t, string(policy), "(allow pod_t rootfs_t (dir (read open mounton)))")
	assert.Contains(t, string(policy), "(allow pod_t pod_containerd_socket_t (sock_file (write)))")
	assert.Contains(t, string(policy), `(filecon "/usr/local/share/kata-containers/kata-containers.img" file kata_guest_image_t)`)
	assert.Contains(t, string(policy), `(filecon "/usr/local/share/kata-containers/vmlinux.container" file kata_guest_image_t)`)
	assert.Contains(t, string(policy), "(allow pod_t kata_guest_image_t (file (read open lock)))")
	assert.Contains(t, string(policy), "(allow pod_t self (io_uring (allowed)))")
	assert.NotContains(t, string(policy), "(allow pod_p pod_containerd_t (unix_stream_socket")
	assert.NotContains(t, string(policy), "(allow pod_t pod_containerd_t (unix_stream_socket (all)))")
	assert.NotContains(t, string(policy), "(allow pod_t init_t (unix_dgram_socket (all)))")
	assert.NotContains(t, string(policy), "(allow pod_t rootfs_t (fs_classes")
	assert.NotContains(t, string(policy), "(allow pod_t usr_t")
	assert.NotContains(t, string(policy), "(allow pod_p usr_t")
	assert.NotContains(t, string(policy), "(allow pod_p self (io_uring")
}

func TestSELinuxIOUringClassMatchesLinux618(t *testing.T) {
	t.Parallel()

	classes, err := os.ReadFile("policy/selinux/immutable/classes.cil")
	require.NoError(t, err)

	// Linux v6.18 appends the setup authorization after the three existing
	// io_uring permissions. Permission ordering is a kernel-policy ABI.
	assert.Contains(t, string(classes), "(class io_uring (override_creds sqpoll cmd allowed))")
}

func TestCRIContainerdMaySetPodOverlayMountContext(t *testing.T) {
	t.Parallel()

	policy, err := os.ReadFile("policy/selinux/services/cri.cil")
	require.NoError(t, err)

	assert.Contains(t, string(policy), "(allow pod_containerd_t fs_t (fs_classes (relabelfrom)))")
	assert.Contains(t, string(policy), "(allow pod_containerd_t tmpfs_t (fs_classes (relabelfrom)))")
	assert.Contains(t, string(policy), "(allow pod_containerd_t devpts_t (fs_classes (relabelfrom)))")
	assert.Contains(t, string(policy), "(allow pod_containerd_t ephemeral_t (fs_classes (relabelfrom relabelto)))")
	assert.Contains(t, string(policy), "(allow ephemeral_t tmpfs_t (filesystem (associate)))")
	assert.Contains(t, string(policy), "(allow ephemeral_t devpts_t (filesystem (associate)))")
	assert.NotContains(t, string(policy), "(typetransition pod_containerd_t ephemeral_t process pod_t)")
	assert.NotContains(t, string(policy), "(typetransition pod_containerd_t containerd_state_t process pod_t)")
	assert.Contains(t, string(policy), "(allow pod_containerd_t ephemeral_t (file (execute execute_no_trans)))")
	assert.Contains(t, string(policy), "(allow pod_p ephemeral_t (file (entrypoint execute)))")
	assert.Contains(t, string(policy), "(typeattributeset mcs_exempt_p pod_containerd_t)")

	machinedPolicy, err := os.ReadFile("policy/selinux/services/machined.cil")
	require.NoError(t, err)
	assert.Contains(t, string(machinedPolicy), "(typeattributeset mcs_exempt_p init_t)")

	kubeletPolicy, err := os.ReadFile("policy/selinux/services/kubelet.cil")
	require.NoError(t, err)
	assert.Contains(t, string(kubeletPolicy), "(typeattributeset mcs_exempt_p kubelet_t)")
}

func TestCRIContainerLabelsHaveUpstreamMCSRange(t *testing.T) {
	t.Parallel()

	mcs, err := os.ReadFile("policy/selinux/common/mcs.cil")
	require.NoError(t, err)
	roles, err := os.ReadFile("policy/selinux/immutable/roles.cil")
	require.NoError(t, err)

	assert.Contains(t, string(mcs), "(category c0)")
	assert.Contains(t, string(mcs), "(category c1023)")
	assert.Contains(t, string(mcs), "(sensitivitycategory s0 (range c0 c1023))")
	assert.Contains(t, string(mcs), "(level systemHigh (s0 (range c0 c1023)))")
	assert.Contains(t, string(roles), "(userrange system_u (systemLow systemHigh))")
}

func TestRestoreFileContextsRepairsStaleOverlayWithoutFollowingSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cniDir := filepath.Join(root, "cni", "bin")
	cniBinary := filepath.Join(cniDir, "cilium-sysctlfix")
	containerdDir := filepath.Join(root, "containerd")
	containerdPlugin := filepath.Join(containerdDir, "containerd-nydus-grpc")
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside")
	symlink := filepath.Join(root, "link")

	for _, dir := range []string{cniDir, containerdDir} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	for _, file := range []string{cniBinary, containerdPlugin, outsideFile} {
		require.NoError(t, os.WriteFile(file, []byte("test"), 0o600))
	}

	require.NoError(t, os.Symlink(outsideDir, symlink))

	seenModes := map[string]fs.FileMode{}
	labels := map[string]string{}

	for _, path := range []string{
		root,
		filepath.Dir(cniDir),
		cniDir,
		cniBinary,
		containerdDir,
		containerdPlugin,
		symlink,
	} {
		labels[path] = "system_u:object_r:unlabeled_t:s0"
	}

	err := restoreFileContexts(
		root,
		func(path string, mode fs.FileMode) (string, bool, error) {
			seenModes[path] = mode

			relativePath, err := filepath.Rel(root, path)
			if err != nil {
				return "", false, err
			}

			policyPath := "/opt"
			if relativePath != "." {
				policyPath = filepath.Join(policyPath, relativePath)
			}

			return LookupFileContext(policyPath, mode)
		},
		func(path, label string, _ ...string) error {
			labels[path] = label

			return nil
		},
	)
	require.NoError(t, err)

	expectedLabels := map[string]string{
		root:                 "system_u:object_r:opt_t:s0",
		filepath.Dir(cniDir): "system_u:object_r:cni_plugin_t:s0",
		cniDir:               "system_u:object_r:cni_plugin_t:s0",
		cniBinary:            "system_u:object_r:cni_plugin_t:s0",
		containerdDir:        "system_u:object_r:containerd_plugin_t:s0",
		containerdPlugin:     "system_u:object_r:containerd_plugin_t:s0",
		symlink:              "system_u:object_r:opt_t:s0",
	}

	for path, expected := range expectedLabels {
		assert.Equal(t, expected, labels[path], "label for %q", path)
	}

	assert.NotContains(t, seenModes, outsideFile, "walk followed symlink")
	assert.NotZero(t, seenModes[symlink]&fs.ModeSymlink, "symlink mode = %v, want ModeSymlink", seenModes[symlink])
}

func TestRestoreFileContextsWrapsErrors(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	sentinel := errors.New("sentinel")

	err := restoreFileContexts(
		root,
		func(string, fs.FileMode) (string, bool, error) {
			return "canonical", true, nil
		},
		func(string, string, ...string) error {
			return sentinel
		},
	)

	require.ErrorIs(t, err, sentinel)
	assert.ErrorContains(t, err, root)
}
