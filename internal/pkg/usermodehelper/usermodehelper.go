// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package usermodehelper bounds what machined serves as the kernel's static
// usermode helper.
//
// With CONFIG_STATIC_USERMODEHELPER the kernel runs every usermode helper as
// /sbin/init, and the SELinux policy moves it into Domain
// (internal/pkg/selinux/policy/selinux/services/usermodehelper.cil). A
// container can make the kernel call request_module, so that domain is
// reachable from workloads: in it, machined serves only the helpers in Names.
package usermodehelper

import (
	"slices"
	"strings"
)

// Domain is the SELinux type the kernel's usermode helper runs in.
const Domain = "usermodehelper_t"

// Names are the argv[0] basenames machined serves in Domain: request_module
// (modprobe) and orderly_poweroff or orderly_reboot (poweroff, shutdown, reboot).
var Names = []string{"modprobe", "poweroff", "shutdown", "reboot"}

// Refused reports whether a process named name (the basename of argv[0]) and
// labelled label (/proc/self/attr/current) must exit before doing anything:
// it runs in Domain under a name that is not a usermode helper.
func Refused(name, label string) bool {
	return Type(label) == Domain && !slices.Contains(Names, name)
}

// Type returns the type field of an SELinux label (user:role:type[:level]),
// or "" when the label has none.
func Type(label string) string {
	fields := strings.SplitN(strings.TrimRight(label, "\x00\n"), ":", 4)
	if len(fields) < 3 {
		return ""
	}

	return fields[2]
}
