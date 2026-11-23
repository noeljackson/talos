// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package usermodehelper_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/siderolabs/talos/internal/pkg/usermodehelper"
)

const helperLabel = "system_u:system_r:usermodehelper_t:s0\x00"

func TestRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		label   string
		refused bool
	}{
		{"modprobe", helperLabel, false},
		{"poweroff", helperLabel, false},
		{"shutdown", helperLabel, false},
		{"reboot", helperLabel, false},
		// machined's own entrypoints and services are never usermode helpers.
		{"init", helperLabel, true},
		{"machined", helperLabel, true},
		{"apid", helperLabel, true},
		{"sandboxd", helperLabel, true},
		{"trustd", helperLabel, true},
		{"dashboard", helperLabel, true},
		{"anything", helperLabel, true},
		// Outside the helper domain the name decides as before.
		{"init", "system_u:system_r:init_t:s0", false},
		{"apid", "system_u:system_r:apid_t:s0", false},
		{"modprobe", "system_u:system_r:kernel_t:s0", false},
		// SELinux disabled or the label unreadable.
		{"init", "", false},
		{"init", "kernel", false},
	} {
		assert.Equal(t, tc.refused, usermodehelper.Refused(tc.name, tc.label), "%q in %q", tc.name, tc.label)
	}
}

func TestType(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "usermodehelper_t", usermodehelper.Type(helperLabel))
	assert.Equal(t, "usermodehelper_t", usermodehelper.Type("system_u:system_r:usermodehelper_t:s0:c1,c2\n"))
	assert.Equal(t, "usermodehelper_t", usermodehelper.Type("system_u:system_r:usermodehelper_t"))
	assert.Empty(t, usermodehelper.Type("unconfined"))
	assert.Empty(t, usermodehelper.Type(""))
}
