// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package filecontext

import (
	"io/fs"
	"strings"
	"testing"
)

func TestMatcherUsesLastMatchAndFileType(t *testing.T) {
	matcher, err := Parse(strings.NewReader(strings.Join([]string{
		"/usr(/.*)? system_u:object_r:usr_t:s0",
		"/usr/bin(/.*)? system_u:object_r:bin_exec_t:s0",
		"/usr/bin/init -- system_u:object_r:init_exec_t:s0",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		path string
		mode fs.FileMode
		want string
	}{
		{"/usr", fs.ModeDir, "system_u:object_r:usr_t:s0"},
		{"/usr/bin/tool", 0, "system_u:object_r:bin_exec_t:s0"},
		{"/usr/bin/init", 0, "system_u:object_r:init_exec_t:s0"},
		{"/usr/bin/init", fs.ModeDir, "system_u:object_r:bin_exec_t:s0"},
	}

	for _, testCase := range testCases {
		got, ok := matcher.Lookup(testCase.path, testCase.mode)
		if !ok || got != testCase.want {
			t.Errorf("Lookup(%q, %v) = %q, %v, want %q, true", testCase.path, testCase.mode, got, ok, testCase.want)
		}
	}
}

func TestParseRejectsUnknownType(t *testing.T) {
	if _, err := Parse(strings.NewReader("/path -x context")); err == nil {
		t.Fatal("expected an error")
	}
}
