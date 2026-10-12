package filter

import (
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
)

// Excluded reports whether a walk that starts at root skips a directory
// because of the exclude entries of the configuration. Every command that
// walks a tree asks it here, so that they agree on what is excluded.
//
// An entry is the name of a directory ("vendor"), a glob for one ("*.cache"),
// several names in a row ("tools/ghidra") or an absolute path, each with or
// without a slash at the end. Names are held
// against the part of the path below root only, and an absolute path counts
// when it lies below root: what is named by hand is not excluded by where it
// is. A directory inside an excluded one is excluded too. Both paths must be
// absolute.
func Excluded(root, dir string, exclude []string) bool {
	sep := string(filepath.Separator)
	root, dir = filepath.Clean(root), filepath.Clean(dir)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+sep) {
		return false
	}
	names := strings.Split(rel, sep)

	for _, entry := range exclude {
		// "vendor/" is how many write a directory
		if trimmed := strings.TrimRight(entry, sep); trimmed != "" {
			entry = trimmed
		}
		if entry == "" {
			continue
		}
		if filepath.IsAbs(entry) {
			entry = filepath.Clean(entry)
			if below(entry, root) && (dir == entry || below(dir, entry)) {
				return true
			}
			continue
		}
		if strings.Contains(sep+rel+sep, sep+entry+sep) {
			return true
		}
		for _, name := range names {
			if matched, err := filepath.Match(entry, name); err == nil && matched {
				return true
			}
		}
	}
	return false
}

// below reports whether a path lies inside a directory
func below(path, dir string) bool {
	if dir == string(filepath.Separator) {
		return path != dir
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// ShouldIndexDirectory checks if a directory should be indexed: it is one of
// the included directories or inside one, and a walk from that one would not
// skip it (see Excluded)
func ShouldIndexDirectory(path string, include []string, exclude []string) (bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}

	for _, includePath := range include {
		root := filepath.Clean(includePath)
		if absPath != root && !below(absPath, root) {
			continue
		}
		if !Excluded(root, absPath, exclude) {
			return true, nil
		}
	}
	return false, nil
}

// ShouldIndexFile checks if a file should be indexed
// Returns: NOT matches_blacklist OR matches_whitelist
// Equivalent: File rejected if matches_blacklist AND NOT matches_whitelist
func ShouldIndexFile(filePath string, blacklist []string, whitelist []string) (bool, error) {
	// Normalize path to absolute
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return false, err
	}

	slog.Debug("Checking file filter", "file", absPath, "blacklist_count", len(blacklist))

	// Check blacklist first
	matchesBlacklist := false
	matchedPattern := ""
	for _, pattern := range blacklist {
		matched, err := regexp.MatchString(pattern, absPath)
		if err != nil {
			slog.Debug("Invalid regex pattern", "pattern", pattern, "error", err)
			continue
		}
		if matched {
			matchesBlacklist = true
			matchedPattern = pattern
			slog.Debug("Matched blacklist pattern", "pattern", pattern, "file", absPath)
			break
		}
	}

	// If doesn't match blacklist, allow
	if !matchesBlacklist {
		slog.Debug("No blacklist match, allowing file", "file", absPath)
		return true, nil
	}

	// Matches blacklist - check if whitelist provides exception
	for _, pattern := range whitelist {
		matched, err := regexp.MatchString(pattern, absPath)
		if err != nil {
			continue
		}
		if matched {
			slog.Debug("Whitelist exception matched, allowing file", "pattern", pattern, "file", absPath)
			return true, nil
		}
	}

	// Matched blacklist and no whitelist exception - reject
	slog.Debug("Rejecting file", "file", absPath, "blacklist_pattern", matchedPattern)
	return false, nil
}

// MatchesPattern checks if a path matches a glob or regex pattern
func MatchesPattern(path string, pattern string) bool {
	// Try as glob pattern first
	matched, err := filepath.Match(pattern, filepath.Base(path))
	if err == nil && matched {
		return true
	}

	// Try as regex pattern
	matched, err = regexp.MatchString(pattern, path)
	if err == nil && matched {
		return true
	}

	// Try literal substring match
	return strings.Contains(path, pattern)
}
