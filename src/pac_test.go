package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPACQuotesProxyHost(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := &App{store: store, pacStore: &pacStore{rules: &pacRules{Suffix: []string{"example.com"}}}}
	host := `1.2.3.4";alert(1);//`
	out := app.buildPAC(host)

	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "var PROXY = ") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("PROXY line not found")
	}
	const prefix = "var PROXY = "
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, ";") {
		t.Fatalf("unexpected PROXY line: %s", line)
	}
	var literal string
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(line, prefix), ";")), &literal); err != nil {
		t.Fatalf("PROXY literal is not valid JSON: %v (%s)", err, line)
	}
	if !strings.Contains(literal, `1.2.3.4";alert(1);//`) {
		t.Fatalf("proxy host was altered: %q", literal)
	}
	if !strings.Contains(line, `\"`) {
		t.Fatalf("double quote was not escaped: %s", line)
	}
}
