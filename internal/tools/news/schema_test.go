package news

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the tool schema snapshots in testdata/toolsnaps")

// TestToolSnapshots pins what MCP clients see of every tool: name,
// description, annotations and input/output JSON schemas. A change to a Go
// struct tag silently changes the API offered to LLMs; this makes it show up
// in review. After an intended change, run:
//
//	go test ./internal/tools/news -run TestToolSnapshots -update
func TestToolSnapshots(t *testing.T) {
	res, err := connect(t, nil).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("testdata", "toolsnaps")
	if *update {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, tl := range res.Tools {
		got, err := json.MarshalIndent(tl, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		name := tl.Name + ".json"
		seen[name] = true
		path := filepath.Join(dir, name)
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no snapshot (%v); run with -update to create it", tl.Name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: schema differs from %s; run with -update if the change is intended\n--- got\n%s", tl.Name, path, got)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !seen[e.Name()] {
			t.Errorf("stale snapshot %s: the tool no longer exists", e.Name())
		}
	}
}
