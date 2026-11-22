package main

import "testing"

func TestCountProxiesFlow(t *testing.T) {
	cfg := `proxies: [{"name":"a","type":"ss","ws-opts":{"path":"/x}]"}},{"name":"b,c","type":"vmess"}]
proxy-groups: [{"name":"G","type":"select","proxies":["a","b,c"]}]
rules:
  - MATCH,G
`
	if got := countProxies(cfg); got != 2 {
		t.Fatalf("countProxies() = %d, want 2", got)
	}
}

func TestCountProxiesFlowMultiLine(t *testing.T) {
	cfg := `proxies: [
  {"name":"a","type":"ss"},
  {"name":"b","type":"ss"}
]
proxy-groups: []
`
	if got := countProxies(cfg); got != 2 {
		t.Fatalf("countProxies() = %d, want 2", got)
	}
}

func TestCountProxiesBlock(t *testing.T) {
	cfg := `proxies:
  - name: a
    type: ss
    server: 127.0.0.1
  - name: b
    type: ss
proxy-groups:
  - name: G
    type: select
`
	if got := countProxies(cfg); got != 2 {
		t.Fatalf("countProxies() = %d, want 2", got)
	}
}

func TestCountProxiesNone(t *testing.T) {
	if got := countProxies("proxy-providers: {}\nrules:\n  - MATCH,DIRECT\n"); got != 0 {
		t.Fatalf("countProxies() = %d, want 0", got)
	}
}
