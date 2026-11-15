package unit_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestTheManifestMatchesWhatThePackageDoes holds arandu.mod.toml against the
// code. This package starts programs (exec) and writes a private temporary
// directory (filesystem), and makes no network call, owns no table and does not
// import the framework. `aru doctor` audits an application's own tree and never
// loads an installed package, so this suite is the only place the declaration is
// checked.
func TestTheManifestMatchesWhatThePackageDoes(t *testing.T) {
	raw, err := os.ReadFile("../../arandu.mod.toml")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(raw)

	want := map[string]string{
		"network":    "false",
		"filesystem": "true",
		"exec":       "true",
		"migrations": "false",
	}
	for key, value := range want {
		pattern := regexp.MustCompile(`(?m)^` + key + `\s*=\s*(\w+)`)
		m := pattern.FindStringSubmatch(manifest)
		if m == nil {
			t.Errorf("arandu.mod.toml does not declare %s", key)
			continue
		}
		if m[1] != value {
			t.Errorf("arandu.mod.toml declares %s = %s, the code needs %s", key, m[1], value)
		}
	}
	if !strings.Contains(manifest, `name = "hyz-is/whisper"`) {
		t.Error(`arandu.mod.toml must name the package "hyz-is/whisper"`)
	}

	// The sources behind the declaration: no network, no framework.
	files, err := filepath.Glob("../../*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources found: %v", err)
	}
	forbidden := []string{"net", "net/http", "database/sql", "github.com/arandu-io/"}
	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if path == bad || (strings.HasSuffix(bad, "/") && strings.HasPrefix(path, bad)) {
					t.Errorf("%s imports %s, which the manifest says this package does not use", filepath.Base(file), path)
				}
			}
		}
	}
}
