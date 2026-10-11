package filter

import (
	"path/filepath"
	"testing"
)

func TestShouldIndexFile(t *testing.T) {
	tests := []struct {
		name      string
		filePath  string
		blacklist []string
		whitelist []string
		want      bool
	}{
		{
			name:      "no filters - allow",
			filePath:  "/home/user/project/file.go",
			blacklist: []string{},
			whitelist: []string{},
			want:      true,
		},
		{
			name:      "matches blacklist, no whitelist - reject",
			filePath:  "/home/user/project/secret.txt",
			blacklist: []string{`.*\.secret$`, `.*secret\.txt$`},
			whitelist: []string{},
			want:      false,
		},
		{
			name:      "matches blacklist AND whitelist - allow (whitelist exception)",
			filePath:  "/home/user/project/important.secret",
			blacklist: []string{`.*\.secret$`},
			whitelist: []string{`.*important\.secret$`},
			want:      true,
		},
		{
			name:      "doesn't match blacklist - allow",
			filePath:  "/home/user/project/main.go",
			blacklist: []string{`.*_test\.go$`},
			whitelist: []string{},
			want:      true,
		},
		{
			name:      "matches blacklist, whitelist doesn't match - reject",
			filePath:  "/home/user/project/test.secret",
			blacklist: []string{`.*\.secret$`},
			whitelist: []string{`.*important\.secret$`},
			want:      false,
		},
		{
			name:      "multiple blacklist patterns",
			filePath:  "/home/user/project/test_file.go",
			blacklist: []string{`.*_test\.go$`, `.*\.tmp$`},
			whitelist: []string{},
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ShouldIndexFile(tt.filePath, tt.blacklist, tt.whitelist)
			if err != nil {
				t.Errorf("ShouldIndexFile() error = %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("ShouldIndexFile() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldIndexDirectory(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		include []string
		exclude []string
		want    bool
	}{
		{
			name:    "in include, not excluded - allow",
			path:    "/home/user/project/src",
			include: []string{"/home/user/project"},
			exclude: []string{"node_modules", ".git"},
			want:    true,
		},
		{
			name:    "not in include - reject",
			path:    "/opt/external",
			include: []string{"/home/user/project"},
			exclude: []string{},
			want:    false,
		},
		{
			name:    "in include but excluded - reject",
			path:    "/home/user/project/node_modules",
			include: []string{"/home/user/project"},
			exclude: []string{"node_modules"},
			want:    false,
		},
		{
			name:    "excluded with a trailing slash - reject",
			path:    "/home/user/project/vendor/lib",
			include: []string{"/home/user/project"},
			exclude: []string{"vendor/"},
			want:    false,
		},
		{
			name:    "in include, different exclude - allow",
			path:    "/home/user/project/src",
			include: []string{"/home/user/project"},
			exclude: []string{"node_modules", "build"},
			want:    true,
		},
		{
			name:    "exact match with include path - allow",
			path:    "/home/user/project",
			include: []string{"/home/user/project"},
			exclude: []string{},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Normalize test paths
			normalizedPath, _ := filepath.Abs(tt.path)
			normalizedInclude := make([]string, len(tt.include))
			for i, p := range tt.include {
				normalizedInclude[i], _ = filepath.Abs(p)
			}

			got, err := ShouldIndexDirectory(normalizedPath, normalizedInclude, tt.exclude)
			if err != nil {
				t.Errorf("ShouldIndexDirectory() error = %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("ShouldIndexDirectory(%s) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestExcluded(t *testing.T) {
	root := "/home/user/project"
	tests := []struct {
		name    string
		dir     string
		exclude []string
		want    bool
	}{
		{"a name", root + "/vendor", []string{"vendor"}, true},
		{"a name, deeper", root + "/a/vendor", []string{"vendor"}, true},
		{"inside an excluded directory", root + "/vendor/lib/x", []string{"vendor"}, true},
		{"part of a name", root + "/vendored", []string{"vendor"}, false},
		{"a glob", root + "/a/x.cache", []string{"*.cache"}, true},
		{"a glob, inside", root + "/x.cache/y", []string{"*.cache"}, true},
		{"names in a row", root + "/tools/ghidra/src", []string{"tools/ghidra"}, true},
		{"names in a row, apart", root + "/tools/x/ghidra", []string{"tools/ghidra"}, false},
		{"an absolute path", root + "/tools/ghidra", []string{root + "/tools/ghidra"}, true},
		{"an absolute path, inside", root + "/tools/ghidra/src", []string{root + "/tools/ghidra"}, true},
		{"an absolute path, beside", root + "/tools/ghidra2", []string{root + "/tools/ghidra"}, false},
		{"an absolute path elsewhere", root + "/tools/ghidra", []string{"/opt/tools/ghidra"}, false},
		{"a name with a slash", root + "/a/vendor", []string{"vendor/"}, true},
		{"names in a row with a slash", root + "/tools/ghidra", []string{"tools/ghidra/"}, true},
		{"an absolute path with a slash", root + "/tools/ghidra/src", []string{root + "/tools/ghidra/"}, true},
		{"a glob with a slash", root + "/x.cache", []string{"*.cache/"}, true},
		{"only a slash", root + "/src", []string{"/"}, false},
		{"nothing excluded", root + "/src", nil, false},
		{"an empty entry", root + "/src", []string{""}, false},
		{"the root itself", root, []string{"project", root}, false},
		{"outside the root", "/home/user/vendor", []string{"vendor"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Excluded(root, tt.dir, tt.exclude); got != tt.want {
				t.Errorf("Excluded(%s, %v) = %v, want %v", tt.dir, tt.exclude, got, tt.want)
			}
		})
	}

	// What is named by hand is not excluded by where it is
	inside := root + "/vendor/lib"
	for _, exclude := range [][]string{{"vendor"}, {root + "/vendor"}, {inside}} {
		if Excluded(inside, inside+"/src", exclude) {
			t.Errorf("Excluded below %s with %v, want it walked", inside, exclude)
		}
	}
	if !Excluded(inside, inside+"/vendor", []string{"vendor"}) {
		t.Error("vendor below a root inside vendor is not excluded")
	}
}

func TestFilterOrder(t *testing.T) {
	// Test that blacklist→whitelist order is correct
	// File matches both blacklist and whitelist should be ALLOWED

	filePath := "/home/user/project/important.secret"
	blacklist := []string{`.*\.secret$`}
	whitelist := []string{`.*important\.secret$`}

	got, err := ShouldIndexFile(filePath, blacklist, whitelist)
	if err != nil {
		t.Fatalf("ShouldIndexFile() error = %v", err)
	}

	if !got {
		t.Error("File matching both blacklist and whitelist should be allowed (whitelist provides exception)")
	}
}
