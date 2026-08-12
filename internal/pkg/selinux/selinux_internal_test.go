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
