// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/siderolabs/talos/internal/pkg/selinux/filecontext"
)

func TestLookupAgainstTalosFileContexts(t *testing.T) {
	fc := filepath.Join("..", "..", "internal", "pkg", "selinux", "policy", "file_contexts")

	matcher, err := filecontext.ParseFile(fc)
	if err != nil {
		t.Fatalf("parse %s: %v", fc, err)
	}

	cases := []struct {
		path string
		mode fs.FileMode
		want string
	}{
		{"/usr/bin/init", 0, "system_u:object_r:init_exec_t:s0"},
		{"/usr/bin/runc", 0, "system_u:object_r:containerd_exec_t:s0"},
		{"/etc", fs.ModeDir, "system_u:object_r:etc_t:s0"},
		{"/etc/cni/00-foo.conf", 0, "system_u:object_r:cni_conf_t:s0"},
		{"/usr/bin/foo", 0, "system_u:object_r:bin_exec_t:s0"},
		{"/usr/lib/modules/somemod.ko", 0, "system_u:object_r:module_t:s0"},
		{"/", fs.ModeDir, "system_u:object_r:rootfs_t:s0"},
	}

	for _, tc := range cases {
		got, ok := matcher.Lookup(tc.path, tc.mode)
		if !ok || got != tc.want {
			t.Errorf("Lookup(%q, %v) = %q, %v, want %q, true", tc.path, tc.mode, got, ok, tc.want)
		}
	}
}
