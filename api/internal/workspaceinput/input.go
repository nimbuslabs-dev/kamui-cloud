// Package workspaceinput defines the inputs accepted by the coordinator and manager.
package workspaceinput

import (
	"regexp"
	"strings"
)

var (
	workspaceID = regexp.MustCompile(`^ws-[0-9a-f]{8,32}$`)
	githubURL   = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9_.-]{1,100}$`)
	branchChars = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,200}$`)
)

func ValidID(value string) bool {
	return workspaceID.MatchString(value)
}

func ValidRepositoryURL(value string) bool {
	if !githubURL.MatchString(value) {
		return false
	}
	repository := strings.TrimSuffix(value[strings.LastIndexByte(value, '/')+1:], ".git")
	return repository != "" && repository != "." && repository != ".."
}

// ValidBranch accepts a conservative subset of Git branch names.
func ValidBranch(value string) bool {
	if !branchChars.MatchString(value) || value == "HEAD" || strings.HasPrefix(value, "-") ||
		strings.HasSuffix(value, ".") || strings.Contains(value, "..") {
		return false
	}
	for part := range strings.SplitSeq(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
