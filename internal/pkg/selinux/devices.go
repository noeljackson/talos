// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package selinux

import (
	"errors"
	"fmt"
)

// earlyDevices are the device nodes the kernel's usermode helper opens: the Go
// runtime opens /dev/null for a closed standard descriptor, and the helper logs
// to /dev/kmsg. devtmpfs creates them before the policy loads, so they read
// device_t until udev's coldplug labels them (hack/udevd/90-selinux.rules), and
// a module the kernel requests in between is refused.
var earlyDevices = []struct {
	path  string
	label string
}{
	{"/dev/null", "system_u:object_r:null_device_t:s0"},
	{"/dev/zero", "system_u:object_r:null_device_t:s0"},
	{"/dev/kmsg", "system_u:object_r:kmsg_device_t:s0"},
}

// LabelEarlyDevices gives the device nodes the usermode helper needs their
// labels before udev's coldplug does. It sets every label it can and returns
// the failures; udev still labels them later.
func LabelEarlyDevices() error {
	var errs error

	for _, device := range earlyDevices {
		if err := SetLabel(device.path, device.label); err != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: %w", device.path, err))
		}
	}

	return errs
}
