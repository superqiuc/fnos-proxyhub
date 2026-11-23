package main

import (
	"strings"
	"testing"
)

func TestApplyGroupTuningMultilineFlow(t *testing.T) {
	cfg := `mixed-port: 7890
proxy-groups: [
  {"name":"A","type":"url-test","proxies":["n1","剩余流量"]},
  {"name":"B","type":"select","proxies":["A"]}
]
rules:
  - MATCH,B
`
	out, skipped := applyGroupTuning(cfg, true, 120)
	if skipped {
		t.Fatal("quoted JSON flow should be tuned")
	}
	if !strings.Contains(out, `"interval":120`) {
		t.Fatalf("interval not applied: %s", out)
	}
	if !strings.Contains(out, `"lazy":false`) {
		t.Fatalf("lazy not disabled: %s", out)
	}
	if strings.Contains(out, "剩余流量") {
		t.Fatalf("info node was not filtered: %s", out)
	}
	if !strings.Contains(out, "mixed-port: 7890") || !strings.Contains(out, "rules:") {
		t.Fatalf("surrounding YAML was damaged: %s", out)
	}
}

func TestApplyGroupTuningBlockStyleUntouched(t *testing.T) {
	cfg := "proxy-groups:\n  - name: A\n    type: url-test\n"
	out, skipped := applyGroupTuning(cfg, true, 120)
	if skipped {
		t.Fatal("block style should not be reported as skipped")
	}
	if out != cfg {
		t.Fatalf("block style should be untouched:\n%s", out)
	}
}

func TestApplyGroupTuningUnquotedFlowSkipped(t *testing.T) {
	cfg := "proxy-groups: [{name: A, type: url-test, proxies: [n1]}]\n"
	out, skipped := applyGroupTuning(cfg, true, 60)
	if !skipped {
		t.Fatal("unquoted YAML flow should be reported as skipped")
	}
	if out != cfg {
		t.Fatalf("unquoted flow should be untouched:\n%s", out)
	}
}

func TestFlowArrayEndHandlesStrings(t *testing.T) {
	s := `[{"name":"a]b","proxies":["x","y"]}]`
	end := flowArrayEnd(s)
	if end != len(s) {
		t.Fatalf("flowArrayEnd() = %d, want %d", end, len(s))
	}
	if flowArrayEnd(`[{"name":"unterminated}`) != -1 {
		t.Fatal("unterminated flow should return -1")
	}
}
