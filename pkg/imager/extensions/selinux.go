// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package extensions

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/siderolabs/talos/internal/pkg/extensions"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

func (builder *Builder) applySystemExtensionSELinuxLabels(extensionList []*extensions.Extension) error {
	if builder.XAttrsMap == nil {
		builder.XAttrsMap = map[string]string{}
	}

	for _, ext := range extensionList {
		if err := filepath.WalkDir(ext.RootfsPath(), func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			relativePath, relErr := filepath.Rel(ext.RootfsPath(), path)
			if relErr != nil {
				return relErr
			}

			extensionPath := "/" + filepath.ToSlash(relativePath)

			label, ok := systemExtensionSELinuxLabel(extensionPath)
			if ok {
				builder.XAttrsMap[path] = label
			}

			return nil
		}); err != nil {
			return fmt.Errorf("error applying SELinux labels to extension %q: %w", ext.Manifest.Metadata.Name, err)
		}
	}

	return nil
}

func systemExtensionSELinuxLabel(path string) (string, bool) {
	labeledPaths := constants.SystemExtensionSELinuxLabeledPaths

	if containerPath, ok := strings.CutPrefix(path, constants.ExtensionServiceRootfsPath+"/"); ok {
		containerName, innerPath, ok := strings.Cut(containerPath, "/")

		if !ok || containerName == "" || innerPath == "" {
			return "", false
		}

		path = "/" + innerPath
		labeledPaths = constants.ExtensionServiceSELinuxLabeledPaths
	}

	var (
		label       string
		matchedSize int
	)

	for _, labeledPath := range labeledPaths {
		if path != labeledPath.Path && !strings.HasPrefix(path, labeledPath.Path+"/") {
			continue
		}

		if len(labeledPath.Path) > matchedSize {
			label = labeledPath.Label
			matchedSize = len(labeledPath.Path)
		}
	}

	return label, matchedSize > 0
}
