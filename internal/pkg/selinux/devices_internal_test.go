// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package selinux

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// The early labels are udev's, so the node reads the same before and after coldplug.
func TestEarlyDevicesMatchUdevRules(t *testing.T) {
	rules, err := os.ReadFile("../../../hack/udevd/90-selinux.rules")
	require.NoError(t, err)

	for _, device := range earlyDevices {
		rule := regexp.MustCompile(`(?m)^KERNEL=="` + regexp.QuoteMeta(filepath.Base(device.path)) + `",SECLABEL\{selinux\}="([^"]+)"$`)

		match := rule.FindSubmatch(rules)
		require.NotNil(t, match, "no udev rule labels %s", device.path)
		require.Equal(t, string(match[1]), device.label, device.path)
	}
}
