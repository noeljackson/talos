// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package usermodehelper_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/siderolabs/talos/internal/pkg/usermodehelper"
)

const helperLabel = "system_u:system_r:usermodehelper_t:s0\x00"

func TestRefused(t *testing.T) {
	t.Parallel()

	unreadable := errors.New("invalid argument")

	for _, tc := range []struct {
		name    string
		process usermodehelper.Process
		refused bool
	}{
		{"modprobe helper", usermodehelper.Process{Name: "modprobe", Label: helperLabel, PID: 42, SELinux: true}, false},
		{"poweroff helper", usermodehelper.Process{Name: "poweroff", Label: helperLabel, PID: 42, SELinux: true}, false},
		{"shutdown helper", usermodehelper.Process{Name: "shutdown", Label: helperLabel, PID: 42, SELinux: true}, false},
		{"reboot helper", usermodehelper.Process{Name: "reboot", Label: helperLabel, PID: 42, SELinux: true}, false},
		// machined's own entrypoints and services are never usermode helpers.
		{"init as helper", usermodehelper.Process{Name: "init", Label: helperLabel, PID: 42, SELinux: true}, true},
		{"machined as helper", usermodehelper.Process{Name: "machined", Label: helperLabel, PID: 42, SELinux: true}, true},
		{"apid as helper", usermodehelper.Process{Name: "apid", Label: helperLabel, PID: 42, SELinux: true}, true},
		{"sandboxd as helper", usermodehelper.Process{Name: "sandboxd", Label: helperLabel, PID: 42, SELinux: true}, true},
		{"trustd as helper", usermodehelper.Process{Name: "trustd", Label: helperLabel, PID: 42, SELinux: true}, true},
		{"dashboard as helper", usermodehelper.Process{Name: "dashboard", Label: helperLabel, PID: 42, SELinux: true}, true},
		{"unknown as helper", usermodehelper.Process{Name: "anything", Label: helperLabel, PID: 42, SELinux: true}, true},
		// Outside the helper domain the name decides as before.
		{"machined", usermodehelper.Process{Name: "init", Label: "system_u:system_r:init_t:s0", PID: 1, SELinux: true}, false},
		{"apid", usermodehelper.Process{Name: "apid", Label: "system_u:system_r:apid_t:s0", PID: 42, SELinux: true}, false},
		{"modprobe outside", usermodehelper.Process{Name: "modprobe", Label: "system_u:system_r:kernel_t:s0", PID: 42, SELinux: true}, false},
		// An unreadable domain under SELinux might be the helper: fail closed,
		// except for PID 1, which is machined itself.
		{"unreadable under SELinux", usermodehelper.Process{Name: "apid", LabelErr: unreadable, PID: 42, SELinux: true}, true},
		{"unreadable helper name under SELinux", usermodehelper.Process{Name: "modprobe", LabelErr: unreadable, PID: 42, SELinux: true}, true},
		{"unreadable PID 1 under SELinux", usermodehelper.Process{Name: "init", LabelErr: unreadable, PID: 1, SELinux: true}, false},
		// Without SELinux the helper domain does not exist.
		{"unreadable without SELinux", usermodehelper.Process{Name: "apid", LabelErr: unreadable, PID: 42}, false},
		{"empty label without SELinux", usermodehelper.Process{Name: "apid", PID: 42}, false},
	} {
		assert.Equal(t, tc.refused, usermodehelper.Refused(tc.process), tc.name)
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
