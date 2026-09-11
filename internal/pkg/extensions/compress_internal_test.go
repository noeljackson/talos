// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package extensions //nolint:testpackage // test deterministic pseudo-flag generation directly

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	extensionsapi "github.com/siderolabs/talos/pkg/machinery/extensions"
)

func TestXattrPseudoFlagsAreDeterministicAndRootBounded(t *testing.T) {
	rootfsPath := t.TempDir()
	pathA := filepath.Join(rootfsPath, "a")
	pathB := filepath.Join(rootfsPath, "b")
	outOfRootPath := filepath.Join(t.TempDir(), "outside")

	for _, path := range []string{pathA, pathB, outOfRootPath} {
		require.NoError(t, os.WriteFile(path, []byte("test"), 0o644))
	}

	ext := &Extension{Extension: extensionsapi.New(rootfsPath, "test", extensionsapi.Manifest{})}

	first, err := ext.xattrPseudoFlags(map[string]string{
		pathB:         "label-b",
		outOfRootPath: "outside-label",
		pathA:         "label-a",
	})
	require.NoError(t, err)

	second, err := ext.xattrPseudoFlags(map[string]string{
		pathA:         "label-a",
		pathB:         "label-b",
		outOfRootPath: "outside-label",
	})
	require.NoError(t, err)

	expected := []string{
		"-xattrs-exclude", ".*",
		"-p", "a x security.selinux=label-a",
		"-p", "b x security.selinux=label-b",
	}

	assert.Equal(t, expected, first)
	assert.Equal(t, expected, second)
}
