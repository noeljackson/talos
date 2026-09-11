// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package startup

import (
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/constants"
)

func TestSystemDirectoriesIncludeCiliumRuntime(t *testing.T) {
	t.Parallel()

	for _, directory := range systemDirectories {
		if directory.path != constants.CiliumRuntimePath {
			continue
		}

		if directory.perm != 0o755 {
			t.Fatalf("unexpected Cilium runtime mode: %o", directory.perm)
		}

		if directory.label != constants.CiliumRuntimeSelinuxLabel {
			t.Fatalf("unexpected Cilium runtime SELinux label: %q", directory.label)
		}

		return
	}

	t.Fatalf("Cilium runtime directory %q is missing", constants.CiliumRuntimePath)
}
