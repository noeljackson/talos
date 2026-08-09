// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package filecontext parses and resolves SELinux file_contexts entries.
package filecontext

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
)

type fileType uint8

const (
	typeAny fileType = iota
	typeReg
	typeDir
	typeLnk
	typeChr
	typeBlk
	typeFifo
	typeSock
)

type rule struct {
	re       *regexp.Regexp
	fileType fileType
	context  string
}

// Matcher resolves SELinux labels using file_contexts ordering semantics.
type Matcher struct {
	rules []rule
}

// Parse parses SELinux file_contexts data from reader.
func Parse(reader io.Reader) (*Matcher, error) {
	var rules []rule

	scanner := bufio.NewScanner(reader)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)

		var (
			pattern, context string
			ft               = typeAny
		)

		switch len(fields) {
		case 2:
			pattern, context = fields[0], fields[1]
		case 3:
			var err error

			if ft, err = parseTypeSpec(fields[1]); err != nil {
				return nil, err
			}

			pattern, context = fields[0], fields[2]
		default:
			return nil, fmt.Errorf("malformed file_contexts line: %q", line)
		}

		re, err := regexp.Compile("^" + pattern + "$")
		if err != nil {
			return nil, fmt.Errorf("compiling pattern %q: %w", pattern, err)
		}

		rules = append(rules, rule{re: re, fileType: ft, context: context})
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return &Matcher{rules: rules}, nil
}

// ParseFile parses SELinux file_contexts data from path.
func ParseFile(path string) (*Matcher, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer file.Close() //nolint:errcheck

	return Parse(file)
}

// Lookup returns the SELinux context for path and mode. libselinux resolves
// file_contexts in reverse and returns the first match, so the last matching
// entry wins.
func (matcher *Matcher) Lookup(path string, mode fs.FileMode) (string, bool) {
	ft := fileTypeOf(mode)

	for _, candidate := range slices.Backward(matcher.rules) {
		if candidate.fileType != typeAny && candidate.fileType != ft {
			continue
		}

		if candidate.re.MatchString(path) {
			return candidate.context, true
		}
	}

	return "", false
}

func parseTypeSpec(value string) (fileType, error) {
	switch value {
	case "--":
		return typeReg, nil
	case "-d":
		return typeDir, nil
	case "-l":
		return typeLnk, nil
	case "-c":
		return typeChr, nil
	case "-b":
		return typeBlk, nil
	case "-p":
		return typeFifo, nil
	case "-s":
		return typeSock, nil
	default:
		return typeAny, fmt.Errorf("unknown file_contexts type spec %q", value)
	}
}

func fileTypeOf(mode fs.FileMode) fileType {
	switch {
	case mode&fs.ModeSymlink != 0:
		return typeLnk
	case mode.IsDir():
		return typeDir
	case mode&fs.ModeDevice != 0:
		if mode&fs.ModeCharDevice != 0 {
			return typeChr
		}

		return typeBlk
	case mode&fs.ModeNamedPipe != 0:
		return typeFifo
	case mode&fs.ModeSocket != 0:
		return typeSock
	default:
		return typeReg
	}
}
