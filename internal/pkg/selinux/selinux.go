// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package selinux provides generic code for managing SELinux.
package selinux

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/pkg/xattr"
	"github.com/siderolabs/go-procfs/procfs"
	"golang.org/x/sys/unix"

	"github.com/siderolabs/talos/internal/pkg/containermode"
	"github.com/siderolabs/talos/internal/pkg/selinux/filecontext"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/xfs"
)

//go:embed policy/policy.33
var policy []byte

//go:embed policy/file_contexts
var fileContexts []byte

var defaultFileContextMatcher = sync.OnceValues(func() (*filecontext.Matcher, error) {
	return filecontext.Parse(bytes.NewReader(fileContexts))
})

// LookupFileContext resolves the canonical Talos SELinux context for path and
// mode. The same resolver is used for every filesystem assembled into a Talos
// image so overlay layers cannot replace correctly labeled base directories
// with unlabeled ones.
func LookupFileContext(path string, mode fs.FileMode) (string, bool, error) {
	matcher, err := defaultFileContextMatcher()
	if err != nil {
		return "", false, err
	}

	context, ok := matcher.Lookup(path, mode)

	return context, ok, nil
}

// IsEnabled checks if SELinux is enabled on the system by reading
// the kernel command line. It returns true if SELinux is enabled,
// otherwise it returns false. It also ensures we're not in a container.
// By default SELinux is disabled.
var IsEnabled = sync.OnceValue(func() bool {
	if containermode.InContainer() {
		return false
	}

	val := procfs.ProcCmdline().Get(constants.KernelParamSELinux).First()

	var selinuxFSPresent bool

	if _, err := os.Stat("/sys/fs/selinux"); err == nil {
		selinuxFSPresent = true
	}

	return val != nil && *val == "1" && selinuxFSPresent
})

// IsEnforcing checks if SELinux is enabled and the mode should be enforcing.
// By default if SELinux is enabled we consider it to be permissive.
var IsEnforcing = sync.OnceValue(func() bool {
	if !IsEnabled() {
		return false
	}

	val := procfs.ProcCmdline().Get(constants.KernelParamSELinuxEnforcing).First()

	return val != nil && *val == "1"
})

// GetLabel gets label for file, directory or symlink (not following symlinks)
// It does not perform the operation in case SELinux is disabled.
func GetLabel(filename string) (string, error) {
	if !IsEnabled() {
		return "", nil
	}

	label, err := xattr.LGet(filename, "security.selinux")
	if err != nil {
		return "", err
	}

	if label == nil {
		return "", nil
	}

	return string(bytes.Trim(label, "\x00\n")), nil
}

// SetLabel sets label for file, directory or symlink (not following symlinks)
// It does not perform the operation in case SELinux is disabled, provided label is empty or already set.
func SetLabel(filename string, label string, excludeLabels ...string) error {
	if label == "" || !IsEnabled() {
		return nil
	}

	currentLabel, err := GetLabel(filename)
	if err != nil {
		return err
	}

	// Skip extra FS transactions when labels are okay.
	if currentLabel == label {
		return nil
	}

	// Skip setting label if it's in excludeLabels.
	if currentLabel != "" && slices.Contains(excludeLabels, currentLabel) {
		return nil
	}

	// We use LGet/LSet so that we manipulate label on the exact path, not the symlink target.
	if err := xattr.LSet(filename, "security.selinux", []byte(label)); err != nil {
		return err
	}

	return nil
}

// FGetLabel gets label for file, directory or symlink (not following symlinks) using provided root.
// It does not perform the operation in case SELinux is disabled.
func FGetLabel(root xfs.Root, filename string) (string, error) {
	if !IsEnabled() {
		return "", nil
	}

	f, err := xfs.OpenFile(root, filename, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck

	osf, err := xfs.AsOSFile(f, filename)
	if err != nil {
		return "", err
	}
	defer osf.Close() //nolint:errcheck

	label, err := xattr.FGet(osf, "security.selinux")
	if err != nil {
		return "", err
	}

	if label == nil {
		return "", nil
	}

	return string(bytes.Trim(label, "\x00\n")), nil
}

// FSetLabel sets label for file, directory or symlink (not following symlinks) using provided root.
// It does not perform the operation in case SELinux is disabled, provided label is empty or already set.
func FSetLabel(root xfs.Root, filename string, label string, excludeLabels ...string) error {
	if label == "" || !IsEnabled() {
		return nil
	}

	currentLabel, err := FGetLabel(root, filename)
	if err != nil {
		return err
	}

	// Skip extra FS transactions when labels are okay.
	if currentLabel == label {
		return nil
	}

	// Skip setting label if it's in excludeLabels.
	if currentLabel != "" && slices.Contains(excludeLabels, currentLabel) {
		return nil
	}

	f, err := xfs.Open(root, filename)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	osf, err := xfs.AsOSFile(f, filename)
	if err != nil {
		return err
	}
	defer osf.Close() //nolint:errcheck

	// We use FGet/FSet so that we manipulate label on the exact path, not the symlink target.
	if err := xattr.FSet(osf, "security.selinux", []byte(label)); err != nil {
		return err
	}

	return nil
}

// SetLabelRecursive sets label for directory and its content recursively.
// It does not perform the operation in case SELinux is disabled, provided label is empty or already set.
func SetLabelRecursive(dir string, label string, excludeLabels ...string) error {
	if label == "" || !IsEnabled() {
		return nil
	}

	return filepath.Walk(dir, func(path string, _ os.FileInfo, err error) error {
		return SetLabel(path, label, excludeLabels...)
	})
}

// RestoreFileContexts recursively restores the canonical Talos SELinux
// contexts below root. Symlinks are labeled but never followed.
func RestoreFileContexts(root string) error {
	if !IsEnabled() {
		return nil
	}

	return restoreFileContexts(root, LookupFileContext, SetLabel)
}

func restoreFileContexts(
	root string,
	lookup func(string, fs.FileMode) (string, bool, error),
	setLabel func(string, string, ...string) error,
) error {
	return filepath.Walk(root, func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}

			return fmt.Errorf("failed to walk %q: %w", path, walkErr)
		}

		label, ok, err := lookup(path, info.Mode())
		if err != nil {
			return fmt.Errorf("failed to look up file context for %q: %w", path, err)
		}

		if !ok {
			return nil
		}

		if err = setLabel(path, label); err != nil {
			// Runtime-created entries may disappear while a tree is being
			// inspected. A replacement will inherit the policy-owned context.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return fmt.Errorf("failed to restore file context for %q: %w", path, err)
		}

		return nil
	})
}

// Init initializes SELinux based on the configured mode.
// It loads the policy and enforces it if necessary.
func Init() error {
	if !IsEnabled() {
		log.Println("selinux: disabled, not loading policy")

		return nil
	}

	if IsEnforcing() {
		log.Println("selinux: running in enforcing mode, policy will be applied as soon as it's loaded")
	}

	log.Println("selinux: loading policy")

	if err := os.WriteFile("/sys/fs/selinux/load", policy, 0o777); err != nil {
		return err
	}

	log.Println("selinux: policy loaded")

	return nil
}
