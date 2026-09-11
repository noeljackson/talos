// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package hack_test

import (
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/opencontainers/selinux/go-selinux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCRIPluginEnablesSELinuxLabelHandling(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile("cri-plugin.part")
	require.NoError(t, err)

	var config struct {
		Plugins map[string]struct {
			EnableSELinux bool `toml:"enable_selinux"`
		} `toml:"plugins"`
	}

	_, err = toml.Decode(string(contents), &config)
	require.NoError(t, err)

	runtimeConfig, ok := config.Plugins["io.containerd.cri.v1.runtime"]
	require.True(t, ok, "CRI runtime plugin config is missing")
	require.True(t, runtimeConfig.EnableSELinux, "CRI must honor Kubernetes SELinux options")
}

func TestCRIHasBaseSELinuxContainerContexts(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile("selinux-containers-contexts")
	require.NoError(t, err)

	contexts := map[string]string{}

	for line := range strings.Lines(string(contents)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		require.True(t, ok, "invalid container context line %q", line)
		contexts[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}

	assert.Equal(t, "system_u:system_r:pod_t:s0", contexts["process"])
	assert.Equal(t, "system_u:object_r:ephemeral_t:s0", contexts["file"])
	assert.Equal(t, contexts["file"], contexts["ro_file"])
	assert.Len(t, contexts, 3)

	processContext, err := selinux.NewContext(contexts["process"])
	require.NoError(t, err)
	processContext["type"] = "cilium_t"
	processContext["level"] = "s0"
	assert.Equal(t, "system_u:system_r:cilium_t:s0", processContext.Get(),
		"Kubernetes SELinux options must be able to derive the dedicated Cilium domain")
	assert.Equal(t, "system_u:object_r:ephemeral_t:s0", contexts["file"],
		"containerd's overlay mount label must be a complete context accepted by the kernel")

	dockerfile, err := os.ReadFile("../Dockerfile")
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(dockerfile),
		"COPY --chmod=0644 hack/selinux-containers-contexts /rootfs/usr/share/containers/selinux/contexts"),
		"container contexts must be copied into both architecture root filesystems",
	)
}
