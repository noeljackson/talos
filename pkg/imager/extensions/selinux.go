// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package extensions

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"
	"go.yaml.in/yaml/v4"

	"github.com/siderolabs/talos/internal/pkg/extensions"
	internalselinux "github.com/siderolabs/talos/internal/pkg/selinux"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	extservices "github.com/siderolabs/talos/pkg/machinery/extensions/services"
)

func (builder *Builder) applySystemExtensionSELinuxLabels(extensionList []*extensions.Extension) error {
	if builder.XAttrsMap == nil {
		builder.XAttrsMap = map[string]string{}
	}

	for _, ext := range extensionList {
		if err := filepath.WalkDir(ext.RootfsPath(), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			relativePath, relErr := filepath.Rel(ext.RootfsPath(), path)
			if relErr != nil {
				return relErr
			}

			extensionPath := "/" + filepath.ToSlash(relativePath)
			if relativePath == "." {
				extensionPath = "/"
			}

			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}

			label, ok, labelErr := systemExtensionSELinuxLabel(extensionPath, info.Mode())
			if labelErr != nil {
				return labelErr
			}

			if ok {
				builder.XAttrsMap[path] = label
			}

			return nil
		}); err != nil {
			return fmt.Errorf("error applying SELinux labels to extension %q: %w", ext.Manifest.Metadata.Name, err)
		}
	}

	for _, ext := range extensionList {
		if err := builder.applyExtensionServiceEntrypointSELinuxLabels(ext, extensionList); err != nil {
			return fmt.Errorf("error applying extension-service entrypoint SELinux labels to extension %q: %w", ext.Manifest.Metadata.Name, err)
		}
	}

	return nil
}

func (builder *Builder) applyExtensionServiceEntrypointSELinuxLabels(ext *extensions.Extension, extensionList []*extensions.Extension) error {
	configPath := filepath.Join(ext.RootfsPath(), strings.TrimPrefix(constants.ExtensionServiceConfigPath, "/"))

	entries, err := os.ReadDir(configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("error reading extension-service configs: %w", err)
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}

		configFilePath := filepath.Join(configPath, entry.Name())
		spec, err := loadExtensionServiceSpec(configFilePath)
		if err != nil {
			return err
		}

		if err = spec.Validate(); err != nil {
			return fmt.Errorf("invalid extension-service config %q: %w", configFilePath, err)
		}

		serviceRootfsPath := filepath.Join(
			ext.RootfsPath(),
			strings.TrimPrefix(constants.ExtensionServiceRootfsPath, "/"),
			spec.Name,
		)

		entrypointPath, err := extensionServiceEntrypointPath(serviceRootfsPath, spec.Container.Entrypoint)
		if err != nil {
			return fmt.Errorf("invalid entrypoint in extension-service config %q: %w", configFilePath, err)
		}

		info, err := os.Lstat(entrypointPath)
		switch {
		case err == nil:
			if err = labelExtensionServiceEntrypoint(builder.XAttrsMap, entrypointPath, info); err != nil {
				return fmt.Errorf("invalid entrypoint in extension-service config %q: %w", configFilePath, err)
			}

			if err = labelExtensionServiceExecutableArguments(builder.XAttrsMap, spec, serviceRootfsPath, extensionList); err != nil {
				return fmt.Errorf("invalid executable argument in extension-service config %q: %w", configFilePath, err)
			}

			continue
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("error inspecting entrypoint from extension-service config %q: %w", configFilePath, err)
		}

		mountedSource, ok, err := extensionServiceMountedEntrypointSource(spec)
		if err != nil {
			return fmt.Errorf("invalid mounted entrypoint in extension-service config %q: %w", configFilePath, err)
		}
		if !ok {
			return fmt.Errorf("entrypoint from extension-service config %q is absent from the service rootfs and is not supplied by a bind mount", configFilePath)
		}

		labeled := false

		for _, candidateExtension := range extensionList {
			candidatePath := filepath.Join(candidateExtension.RootfsPath(), strings.TrimPrefix(mountedSource, "/"))

			candidateInfo, candidateErr := os.Lstat(candidatePath)
			switch {
			case candidateErr == nil:
				if candidateErr = labelExtensionServiceEntrypoint(builder.XAttrsMap, candidatePath, candidateInfo); candidateErr != nil {
					return fmt.Errorf("invalid mounted entrypoint in extension-service config %q: %w", configFilePath, candidateErr)
				}

				labeled = true
			case !errors.Is(candidateErr, fs.ErrNotExist):
				return fmt.Errorf("error inspecting mounted entrypoint from extension-service config %q: %w", configFilePath, candidateErr)
			}
		}

		if !labeled {
			return fmt.Errorf("entrypoint from extension-service config %q is absent from the service rootfs and mounted extension sources", configFilePath)
		}

		if err = labelExtensionServiceExecutableArguments(builder.XAttrsMap, spec, serviceRootfsPath, extensionList); err != nil {
			return fmt.Errorf("invalid executable argument in extension-service config %q: %w", configFilePath, err)
		}
	}

	return nil
}

func labelExtensionServiceExecutableArguments(
	xattrs map[string]string,
	spec extservices.Spec,
	serviceRootfsPath string,
	extensionList []*extensions.Extension,
) error {
	for _, argument := range spec.Container.Args {
		if !filepath.IsAbs(argument) {
			continue
		}

		containerPath, err := extensionServiceContainerPath(argument)
		if err != nil {
			return err
		}

		argumentPath := filepath.Join(serviceRootfsPath, strings.TrimPrefix(containerPath, "/"))
		info, err := os.Lstat(argumentPath)
		switch {
		case err == nil:
			if extensionServiceArgumentIsExecutable(info) {
				xattrs[argumentPath] = constants.SystemExtensionBinSELinuxLabel
			}

			continue
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("error inspecting argument %q: %w", argument, err)
		}

		mountedSource, ok, err := extensionServiceMountedPathSource(spec, containerPath)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}

		for _, candidateExtension := range extensionList {
			candidatePath := filepath.Join(candidateExtension.RootfsPath(), strings.TrimPrefix(mountedSource, "/"))

			candidateInfo, candidateErr := os.Lstat(candidatePath)
			switch {
			case candidateErr == nil:
				if extensionServiceArgumentIsExecutable(candidateInfo) {
					xattrs[candidatePath] = constants.SystemExtensionBinSELinuxLabel
				}
			case !errors.Is(candidateErr, fs.ErrNotExist):
				return fmt.Errorf("error inspecting mounted argument %q: %w", argument, candidateErr)
			}
		}
	}

	return nil
}

func extensionServiceArgumentIsExecutable(info fs.FileInfo) bool {
	return info.Mode()&fs.ModeSymlink != 0 || info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func labelExtensionServiceEntrypoint(xattrs map[string]string, path string, info fs.FileInfo) error {
	if !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
		return fmt.Errorf("entrypoint %q is not a regular file or symlink", path)
	}

	xattrs[path] = constants.SystemExtensionBinSELinuxLabel

	return nil
}

func extensionServiceMountedEntrypointSource(spec extservices.Spec) (string, bool, error) {
	entrypoint, err := extensionServiceContainerPath(spec.Container.Entrypoint)
	if err != nil {
		return "", false, err
	}

	return extensionServiceMountedPathSource(spec, entrypoint)
}

func extensionServiceMountedPathSource(spec extservices.Spec, containerPath string) (string, bool, error) {
	cleanedContainerPath, err := extensionServiceContainerPath(containerPath)
	if err != nil {
		return "", false, err
	}

	var matchedMount *specs.Mount

	for i := range spec.Container.Mounts {
		mount := &spec.Container.Mounts[i]
		if mount.Type != "" && mount.Type != "bind" {
			continue
		}

		destination := filepath.Clean(mount.Destination)
		if !filepath.IsAbs(destination) {
			return "", false, fmt.Errorf("mount destination %q is not absolute", mount.Destination)
		}

		if cleanedContainerPath != destination && !strings.HasPrefix(cleanedContainerPath, destination+string(os.PathSeparator)) {
			continue
		}

		if matchedMount == nil || len(destination) > len(filepath.Clean(matchedMount.Destination)) {
			matchedMount = mount
		}
	}

	if matchedMount == nil {
		return "", false, nil
	}

	if !filepath.IsAbs(matchedMount.Source) {
		return "", false, fmt.Errorf("mount source %q is not absolute", matchedMount.Source)
	}

	destination := filepath.Clean(matchedMount.Destination)
	relativePath := strings.TrimPrefix(strings.TrimPrefix(cleanedContainerPath, destination), string(os.PathSeparator))
	source := filepath.Join(filepath.Clean(matchedMount.Source), relativePath)

	return source, true, nil
}

func loadExtensionServiceSpec(path string) (extservices.Spec, error) {
	var spec extservices.Spec

	file, err := os.Open(path)
	if err != nil {
		return spec, fmt.Errorf("error opening extension-service config %q: %w", path, err)
	}

	defer file.Close() //nolint:errcheck

	if err = yaml.NewDecoder(file).Decode(&spec); err != nil {
		return spec, fmt.Errorf("error decoding extension-service config %q: %w", path, err)
	}

	return spec, nil
}

func extensionServiceEntrypointPath(rootfsPath, entrypoint string) (string, error) {
	cleaned, err := extensionServiceContainerPath(entrypoint)
	if err != nil {
		return "", err
	}

	cleaned = strings.TrimPrefix(cleaned, string(os.PathSeparator))

	return filepath.Join(rootfsPath, cleaned), nil
}

func extensionServiceContainerPath(entrypoint string) (string, error) {
	cleaned := filepath.Clean(entrypoint)

	if cleaned == "" || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("entrypoint %q escapes or names the service rootfs", entrypoint)
	}

	if !filepath.IsAbs(cleaned) {
		cleaned = string(os.PathSeparator) + cleaned
	}

	return cleaned, nil
}

func systemExtensionSELinuxLabel(path string, mode fs.FileMode) (string, bool, error) {
	if containerPath, ok := strings.CutPrefix(path, constants.ExtensionServiceRootfsPath+"/"); ok {
		containerName, innerPath, hasInnerPath := strings.Cut(containerPath, "/")

		if containerName == "" {
			return "", false, nil
		}

		path = "/"
		if hasInnerPath && innerPath != "" {
			path += innerPath
		}

		if label, ok := labelFromOwnedPaths(path, constants.ExtensionServiceSELinuxLabeledPaths); ok {
			return label, true, nil
		}
	}

	return internalselinux.LookupFileContext(path, mode)
}

func labelFromOwnedPaths(path string, labeledPaths []constants.SELinuxLabeledPath) (string, bool) {
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
