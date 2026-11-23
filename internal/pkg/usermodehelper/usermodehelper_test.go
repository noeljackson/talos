// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package usermodehelper_test

import (
	"io/fs"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/siderolabs/talos/internal/pkg/usermodehelper"
)

const helperLabel = "system_u:system_r:usermodehelper_t:s0\x00"

func TestRefused(t *testing.T) {
	t.Parallel()

	// os.ReadFile returns the errno wrapped in a *fs.PathError.
	readErr := func(errno syscall.Errno) error {
		return &fs.PathError{Op: "read", Path: "/proc/self/attr/current", Err: errno}
	}

	for _, tc := range []struct {
		name    string
		process usermodehelper.Process
		refused bool
	}{
		{"modprobe helper", usermodehelper.Process{Name: "modprobe", Label: helperLabel, PID: 42}, false},
		{"poweroff helper", usermodehelper.Process{Name: "poweroff", Label: helperLabel, PID: 42}, false},
		{"shutdown helper", usermodehelper.Process{Name: "shutdown", Label: helperLabel, PID: 42}, false},
		{"reboot helper", usermodehelper.Process{Name: "reboot", Label: helperLabel, PID: 42}, false},
		// machined's own entrypoints and services are never usermode helpers.
		{"init as helper", usermodehelper.Process{Name: "init", Label: helperLabel, PID: 42}, true},
		{"machined as helper", usermodehelper.Process{Name: "machined", Label: helperLabel, PID: 42}, true},
		{"apid as helper", usermodehelper.Process{Name: "apid", Label: helperLabel, PID: 42}, true},
		{"sandboxd as helper", usermodehelper.Process{Name: "sandboxd", Label: helperLabel, PID: 42}, true},
		{"trustd as helper", usermodehelper.Process{Name: "trustd", Label: helperLabel, PID: 42}, true},
		{"dashboard as helper", usermodehelper.Process{Name: "dashboard", Label: helperLabel, PID: 42}, true},
		{"unknown as helper", usermodehelper.Process{Name: "anything", Label: helperLabel, PID: 42}, true},
		// Outside the helper domain the name decides as before.
		{"machined", usermodehelper.Process{Name: "init", Label: "system_u:system_r:init_t:s0", PID: 1}, false},
		{"apid", usermodehelper.Process{Name: "apid", Label: "system_u:system_r:apid_t:s0", PID: 42}, false},
		{"modprobe outside", usermodehelper.Process{Name: "modprobe", Label: "system_u:system_r:kernel_t:s0", PID: 42}, false},
		{"another security module's label", usermodehelper.Process{Name: "apid", Label: "unconfined", PID: 42}, false},
		// EINVAL: no security module provides the attribute, so there is no helper domain.
		{"EINVAL", usermodehelper.Process{Name: "apid", LabelErr: readErr(syscall.EINVAL), PID: 42}, false},
		{"EINVAL helper name", usermodehelper.Process{Name: "modprobe", LabelErr: readErr(syscall.EINVAL), PID: 42}, false},
		// Any other error might hide the helper: fail closed, except for PID 1.
		{"EACCES", usermodehelper.Process{Name: "apid", LabelErr: readErr(syscall.EACCES), PID: 42}, true},
		{"EACCES helper name", usermodehelper.Process{Name: "modprobe", LabelErr: readErr(syscall.EACCES), PID: 42}, true},
		{"EACCES at PID 1", usermodehelper.Process{Name: "init", LabelErr: readErr(syscall.EACCES), PID: 1}, false},
		{"ENOENT", usermodehelper.Process{Name: "apid", LabelErr: readErr(syscall.ENOENT), PID: 42}, true},
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
