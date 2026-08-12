package ui

import (
	"fmt"
	"path"
	"strings"
)

func changedFilePath(change fileChangeMeta) (string, bool) {
	if strings.TrimSpace(change.Path) == "" {
		return "", false
	}
	if change.Additions == 0 && change.Deletions == 0 && change.Operation != "created" {
		return "", false
	}
	return cleanDisplayPath(change.Path), true
}

func appendUniquePath(paths []string, candidate string) []string {
	for _, existing := range paths {
		if existing == candidate {
			return paths
		}
	}
	return append(paths, candidate)
}

func formatChangedFiles(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	type pathGroup struct {
		dir   string
		items []string
	}
	var groups []pathGroup
	for _, value := range paths {
		cleaned := cleanDisplayPath(value)
		dir, item := path.Dir(cleaned), path.Base(cleaned)
		index := -1
		for current := range groups {
			if groups[current].dir == dir {
				index = current
				break
			}
		}
		if index < 0 {
			groups = append(groups, pathGroup{dir: dir})
			index = len(groups) - 1
		}
		groups[index].items = append(groups[index].items, item)
	}

	details := make([]string, 0, len(groups))
	for _, group := range groups {
		if group.dir == "." || group.dir == "" {
			details = append(details, strings.Join(group.items, ", "))
		} else if len(group.items) == 1 {
			details = append(details, path.Join(group.dir, group.items[0]))
		} else {
			details = append(details, strings.TrimSuffix(group.dir, "/")+"/ → "+strings.Join(group.items, ", "))
		}
	}
	noun := "files"
	if len(paths) == 1 {
		noun = "file"
	}
	return fmt.Sprintf("changed %d %s · %s", len(paths), noun, strings.Join(details, " · "))
}
