//go:build !unix

package skills

import (
	"errors"
	"os"
)

func openSource(path string) (*os.Root, error) {
	return nil, &os.PathError{Op: "open skill source", Path: path, Err: errors.ErrUnsupported}
}

func openManifest(root *os.Root) (*os.File, error) {
	return nil, &os.PathError{Op: "open", Path: "SKILL.md", Err: errors.ErrUnsupported}
}
