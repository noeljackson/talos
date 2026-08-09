// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package main builds a SELinux-labeled squashfs image without requiring
// fakeroot or write access to security.* xattrs on the source tree.
//
// It walks the source rootfs, looks up each path's SELinux context against
// the supplied file_contexts, and emits a mksquashfs pseudo-file definition
// list. mksquashfs is then invoked with -xattrs-exclude '.*' so it ignores
// any xattrs on the source filesystem and -pf <pseudo> so it embeds the
// SELinux labels directly into the image.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/siderolabs/talos/internal/pkg/selinux/filecontext"
)

func writePseudo(w *os.File, rootDir string, matcher *filecontext.Matcher) error {
	walkErr := filepath.WalkDir(rootDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(rootDir, p)
		if err != nil {
			return err
		}

		imgPath := "/" + rel
		if rel == "." {
			imgPath = "/"
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		ctx, ok := matcher.Lookup(imgPath, info.Mode())
		if !ok {
			return nil
		}

		b64 := base64.StdEncoding.EncodeToString([]byte(ctx + "\x00"))
		_, err = fmt.Fprintf(w, "%s x security.selinux=0s%s\n", imgPath, b64)

		return err
	})

	return walkErr
}

func run(ctx context.Context) error {
	if len(os.Args) != 5 {
		return fmt.Errorf("usage: %s <root_dir> <output_image> <file_contexts> <compression_level>", os.Args[0])
	}

	rootDir, output, fcPath, level := os.Args[1], os.Args[2], os.Args[3], os.Args[4]

	matcher, err := filecontext.ParseFile(fcPath)
	if err != nil {
		return fmt.Errorf("parse file_contexts: %w", err)
	}

	pseudo, err := os.CreateTemp("", "labeled-squashfs-pseudo-*")
	if err != nil {
		return fmt.Errorf("create pseudo file: %w", err)
	}
	defer os.Remove(pseudo.Name()) //nolint:errcheck

	if err := writePseudo(pseudo, rootDir, matcher); err != nil {
		pseudo.Close() //nolint:errcheck

		return fmt.Errorf("emit pseudo definitions: %w", err)
	}

	if err := pseudo.Close(); err != nil {
		return fmt.Errorf("close pseudo file: %w", err)
	}

	cmd := exec.CommandContext(
		ctx,
		"mksquashfs",
		rootDir, output,
		"-all-root", "-noappend",
		"-comp", "zstd", "-Xcompression-level", level,
		"-no-progress",
		"-xattrs-exclude", ".*",
		"-pf", pseudo.Name(),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mksquashfs: %w", err)
	}

	return nil
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "labeled-squashfs:", err)
		os.Exit(1)
	}
}
