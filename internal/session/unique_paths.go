package session

import (
	"fmt"
	"sort"
)

// TreePath is a session's place in the tree: group path + "/" + title.
func TreePath(group, title string) string {
	if group == "" {
		group = DefaultGroupPath
	}
	return group + "/" + title
}

// PathTakenError refuses a create, rename or move onto a tree path another
// session already holds while unique_paths is on.
type PathTakenError struct {
	Path  string
	Other *Instance
}

func (e *PathTakenError) Error() string {
	return fmt.Sprintf("%s is already taken by session %q (id: %s); unique_paths is on", e.Path, e.Other.Title, e.Other.ID)
}

// CheckUniquePath reports a PathTakenError when unique_paths is on and another
// session than self already holds title in group. Same titles in different
// groups stay allowed.
func CheckUniquePath(instances []*Instance, self *Instance, group, title string) error {
	if !uniquePathsEnabled() {
		return nil
	}
	path := TreePath(group, title)
	for _, other := range instances {
		if other == nil || (self != nil && other.ID == self.ID) {
			continue
		}
		if TreePath(other.GroupPath, other.Title) == path {
			return &PathTakenError{Path: path, Other: other}
		}
	}
	return nil
}

func uniquePathsEnabled() bool {
	cfg, err := LoadUserConfig()
	return err == nil && cfg != nil && cfg.GetUniquePaths()
}

// PathDuplicate is one tree path held by more than one session.
type PathDuplicate struct {
	Path     string
	Sessions []*Instance
}

// DuplicatePaths lists every tree path held by more than one session, sorted
// by path. Existing duplicates are only reported, never renamed.
func DuplicatePaths(instances []*Instance) []PathDuplicate {
	byPath := map[string][]*Instance{}
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		path := TreePath(inst.GroupPath, inst.Title)
		byPath[path] = append(byPath[path], inst)
	}
	var dups []PathDuplicate
	for path, sessions := range byPath {
		if len(sessions) > 1 {
			dups = append(dups, PathDuplicate{Path: path, Sessions: sessions})
		}
	}
	sort.Slice(dups, func(a, b int) bool { return dups[a].Path < dups[b].Path })
	return dups
}
