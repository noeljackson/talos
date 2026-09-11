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

	pathrs "github.com/cyphar/filepath-securejoin/pathrs-lite"
	"github.com/cyphar/filepath-securejoin/pathrs-lite/procfs"
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
		if err := prepareExtensionServiceRootfsMountpoints(ext, extensionList); err != nil {
			return fmt.Errorf("error preparing extension-service rootfs mountpoints for extension %q: %w", ext.Manifest.Metadata.Name, err)
		}
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

func prepareExtensionServiceRootfsMountpoints(ext *extensions.Extension, extensionList []*extensions.Extension) error {
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

		if spec.RunnerMode == extservices.RunnerModeHost {
			// Host runners have no private rootfs or container mounts. Their
			// executables retain the canonical host file-context policy.
			continue
		}

		serviceRootfsPath := filepath.Join(
			strings.TrimPrefix(constants.ExtensionServiceRootfsPath, "/"),
			spec.Name,
		)

		mountpoints := extensions.ImplicitServiceRootfsMountpoints()

		for _, mount := range spec.Container.Mounts {
			if extensions.ServiceRootfsMountpointIsRuntimeManaged(mount.Destination) {
				continue
			}

			directory, found, err := extensionServiceMountSourceIsDirectory(mount.Source, extensionList)
			if err != nil {
				return fmt.Errorf("error inspecting source for mount destination %q in extension-service config %q: %w", mount.Destination, configFilePath, err)
			}

			mountpoints = append(mountpoints, extensions.ServiceRootfsMountpoint{
				Destination: mount.Destination,
				Directory:   directory,
				TypeUnknown: !found,
			})
		}

		if err = extensions.EnsureServiceRootfsMountpointsInRoot(ext.RootfsPath(), serviceRootfsPath, mountpoints); err != nil {
			return fmt.Errorf("invalid mountpoint in extension-service config %q: %w", configFilePath, err)
		}
	}

	return nil
}

func extensionServiceMountSourceIsDirectory(source string, extensionList []*extensions.Extension) (directory, found bool, err error) {
	cleaned := filepath.Clean(source)
	if !filepath.IsAbs(cleaned) {
		return false, false, fmt.Errorf("mount source %q is not absolute", source)
	}

	for _, candidateExtension := range extensionList {
		rootfs, err := os.Open(candidateExtension.RootfsPath())
		if err != nil {
			return false, false, fmt.Errorf("error opening extension rootfs: %w", err)
		}

		handle, err := pathrs.OpenatInRoot(rootfs, strings.TrimPrefix(cleaned, "/"))
		_ = rootfs.Close()

		switch {
		case err == nil:
			info, statErr := handle.Stat()
			_ = handle.Close()
			if statErr != nil {
				return false, false, fmt.Errorf("error inspecting extension mount source %q: %w", source, statErr)
			}

			found = true
			directory = info.IsDir()
		case errors.Is(err, fs.ErrNotExist):
			continue
		default:
			return false, false, fmt.Errorf("error inspecting extension mount source %q: %w", source, err)
		}
	}

	// Base-image and runtime-created sources are unavailable at this stage.
	// Their actual shape is resolved by machined before starting the service.
	return directory, found, nil
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

		if spec.RunnerMode == extservices.RunnerModeHost {
			// Do not override specialized host entrypoint labels (including
			// pre-shutdown hooks) with the container entrypoint label.
			continue
		}

		serviceRootfsPath := filepath.Join(
			ext.RootfsPath(),
			strings.TrimPrefix(constants.ExtensionServiceRootfsPath, "/"),
			spec.Name,
		)

		entrypoints, err := extensionServiceFiles(spec, spec.Container.Entrypoint, serviceRootfsPath, extensionList)
		if err != nil {
			return fmt.Errorf("invalid entrypoint in extension-service config %q: %w", configFilePath, err)
		}

		if len(entrypoints) == 0 {
			return fmt.Errorf("entrypoint from extension-service config %q is absent from the service rootfs and mounted extension sources", configFilePath)
		}

		for _, entrypoint := range entrypoints {
			if err = labelExtensionServiceEntrypoint(builder.XAttrsMap, entrypoint.path, entrypoint.info); err != nil {
				return fmt.Errorf("invalid entrypoint in extension-service config %q: %w", configFilePath, err)
			}
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

		files, err := extensionServiceFiles(spec, argument, serviceRootfsPath, extensionList)
		if err != nil {
			return err
		}

		for _, file := range files {
			if extensionServiceArgumentIsExecutable(file.info) {
				xattrs[file.path] = constants.SystemExtensionBinSELinuxLabel
			}
		}
	}

	return nil
}

func extensionServiceArgumentIsExecutable(info fs.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func labelExtensionServiceEntrypoint(xattrs map[string]string, path string, info fs.FileInfo) error {
	if !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
		return fmt.Errorf("entrypoint %q is not a regular file or symlink", path)
	}

	xattrs[path] = constants.SystemExtensionBinSELinuxLabel

	return nil
}

type extensionServiceFile struct {
	path string
	info fs.FileInfo
}

// extensionServiceFiles resolves the files visible at a container path. Bind
// sources take precedence over rootfs placeholders or shadowed files. Resolve
// symlinks within each source root, labeling the executable target itself.
func extensionServiceFiles(spec extservices.Spec, containerPath, serviceRootfsPath string, extensionList []*extensions.Extension) ([]extensionServiceFile, error) {
	path, err := extensionServiceContainerPath(containerPath)
	if err != nil {
		return nil, err
	}

	source, mounted, err := extensionServiceMountedPathSource(spec, path)
	if err != nil {
		return nil, err
	}

	roots := []string{serviceRootfsPath}
	if mounted {
		path = source
		roots = make([]string, 0, len(extensionList))

		for _, ext := range extensionList {
			roots = append(roots, ext.RootfsPath())
		}
	}

	var files []extensionServiceFile

	for _, root := range roots {
		file, err := extensionServiceFileInRoot(root, path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("error resolving extension-service path %q: %w", containerPath, err)
		}

		files = append(files, file)
	}

	return files, nil
}

func extensionServiceFileInRoot(root, path string) (extensionServiceFile, error) {
	rootHandle, err := os.Open(root)
	if err != nil {
		return extensionServiceFile{}, err
	}
	defer rootHandle.Close() //nolint:errcheck

	handle, err := pathrs.OpenatInRoot(rootHandle, path)
	if err != nil {
		return extensionServiceFile{}, err
	}
	defer handle.Close() //nolint:errcheck

	info, err := handle.Stat()
	if err != nil {
		return extensionServiceFile{}, err
	}

	resolvedPath, err := procfs.ProcSelfFdReadlink(handle)
	if err != nil {
		return extensionServiceFile{}, err
	}

	resolvedRoot, err := procfs.ProcSelfFdReadlink(rootHandle)
	if err != nil {
		return extensionServiceFile{}, err
	}

	relativePath, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || !filepath.IsLocal(relativePath) {
		return extensionServiceFile{}, fmt.Errorf("resolved extension-service path %q is outside root %q", resolvedPath, resolvedRoot)
	}

	// Keep the extraction root's spelling so the key matches the other xattr
	// entries even when a parent of the extraction directory is a symlink.
	return extensionServiceFile{path: filepath.Join(root, relativePath), info: info}, nil
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
