// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package hack_test

import (
	"os"
	"testing"

	"github.com/BurntSushi/toml"
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
