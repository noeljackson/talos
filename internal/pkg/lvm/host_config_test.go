// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package lvm_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The host lvm.conf the image ships keeps LVM off workload devices: a device
// is rejected when every one of its names that matches a pattern matches an
// "r" pattern, and a rejected device is never opened.
func TestHostConfigRejectsWorkloadDevicesOnly(t *testing.T) {
	t.Parallel()

	conf, err := os.ReadFile("../../../hack/lvm.conf")
	require.NoError(t, err)

	filter := regexp.MustCompile(`(?m)^\s*global_filter\s*=\s*\[(.*)\]\s*$`).FindSubmatch(conf)
	require.NotNil(t, filter, "hack/lvm.conf has no devices/global_filter")

	var reject []*regexp.Regexp

	for _, quoted := range regexp.MustCompile(`"([^"]*)"`).FindAllSubmatch(filter[1], -1) {
		pattern := string(quoted[1])
		require.True(t, strings.HasPrefix(pattern, "r|") && strings.HasSuffix(pattern, "|"), "only reject patterns: %s", pattern)

		reject = append(reject, regexp.MustCompile(pattern[2:len(pattern)-1]))
	}

	rejected := func(path string) bool {
		for _, re := range reject {
			if re.MatchString(path) {
				return true
			}
		}

		return false
	}

	for _, path := range []string{
		"/dev/drbd1001",
		"/dev/drbd/by-res/pvc-0a3ad3d6-e3ab-4037-93b5-dc4a926c5d83/0",
		"/dev/loop12",
		"/dev/mapper/Linstor-Crypt-pvc-0a3ad3d6-e3ab-4037-93b5-dc4a926c5d83_00000",
		"/dev/disk/by-id/dm-name-Linstor-Crypt-pvc-0a3ad3d6-e3ab-4037-93b5-dc4a926c5d83_00000",
	} {
		assert.True(t, rejected(path), "%s stays visible to the host's LVM", path)
	}

	for _, path := range []string{
		"/dev/nvme0n1p1",
		"/dev/nvme1n1",
		"/dev/sda3",
		"/dev/vda4",
		"/dev/md127",
		"/dev/dm-3",
		"/dev/mapper/vg0-data",
	} {
		assert.False(t, rejected(path), "%s is hidden from the host's LVM", path)
	}
}
