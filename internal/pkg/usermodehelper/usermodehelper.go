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
	"errors"
	"log"
	"os"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/siderolabs/go-kmsg"
	"golang.org/x/sys/unix"
)

// Domain is the SELinux type the kernel's usermode helper runs in.
const Domain = "usermodehelper_t"

// LabelPath is the calling thread's own SELinux label. Not /proc/self: that is
// the thread-group leader, and when another thread reads it the kernel checks
// process getattr, which the helper's domain does not have, so the read fails
// with EACCES and the helper refuses itself.
const LabelPath = "/proc/thread-self/attr/current"

// OwnLabel reads this thread's label. The thread stays locked between the open
// and the read, since the open resolves thread-self to the thread that makes it.
func OwnLabel() ([]byte, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	return os.ReadFile(LabelPath)
}

// Names are the argv[0] basenames machined serves in Domain: request_module
// (modprobe) and orderly_poweroff or orderly_reboot (poweroff, shutdown, reboot).
var Names = []string{"modprobe", "poweroff", "shutdown", "reboot"}

// Process is what machined knows about itself when it starts.
type Process struct {
	// Name is the basename of argv[0].
	Name string
	// Label is the label OwnLabel read, and LabelErr the error reading it.
	Label    string
	LabelErr error
	// PID is the process ID; PID 1 is machined itself, never a helper.
	PID int
}

// Refused reports whether the process must exit before doing anything: it runs
// in Domain under a name that is not a usermode helper, or its domain cannot be
// read, so it might be the helper. It fails closed: only EINVAL, which means no
// security module provides the attribute and so there is no helper domain, and
// PID 1 are served without a label. The helper's domain can read neither
// /proc/cmdline nor /sys/fs/selinux, so asking whether SELinux is enabled
// would fail open in exactly that domain.
func Refused(p Process) bool {
	if p.LabelErr != nil {
		return !errors.Is(p.LabelErr, syscall.EINVAL) && p.PID != 1
	}

	return Type(p.Label) == Domain && !slices.Contains(Names, p.Name)
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

// SetupLogger sends the standard logger to the kernel log, opening /dev/kmsg
// write-only: a helper only writes the kernel log, and opening it for reading
// as well would need CAP_SYSLOG.
func SetupLogger(prefix string) error {
	f, err := os.OpenFile("/dev/kmsg", os.O_WRONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return err
	}

	log.SetOutput(&kmsg.Writer{KmsgWriter: f})
	log.SetPrefix(prefix + " ")
	log.SetFlags(0)

	return nil
}
