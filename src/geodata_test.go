package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeoNeeds(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want []string
	}{
		{"none", "proxies: []\nrules:\n  - MATCH,DIRECT\n", nil},
		{"geoip", "rules:\n  - GEOIP,CN,DIRECT\n", []string{"Country.mmdb"}},
		{"geosite", "rules:\n  - GEOSITE,cn,PROXY\n", []string{"GeoSite.dat"}},
		{
			"both",
			"rules:\n  - GEOSITE,cn,PROXY\n  - GEOIP,CN,DIRECT\n",
			[]string{"GeoSite.dat", "Country.mmdb"},
		},
		{
			"geodata-mode",
			"geodata-mode: true\nrules:\n  - GEOIP,CN,DIRECT\n",
			[]string{"GeoIP.dat"},
		},
		{
			"asn",
			"rules:\n  - GEOIP,ASN,DIRECT\n",
			[]string{"Country.mmdb", "ASN.mmdb"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := geoNeeds(tc.cfg)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("geoNeeds() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGeoURLsPrefersMirror(t *testing.T) {
	urls := geoURLs("country.mmdb")
	if len(urls) < 2 {
		t.Fatalf("unexpected urls: %v", urls)
	}
	if !strings.Contains(urls[0], "cdn.jsdelivr.net") {
		t.Fatalf("first url should be jsDelivr mirror, got %s", urls[0])
	}
	if !strings.Contains(urls[len(urls)-1], "github.com") {
		t.Fatalf("last url should be GitHub fallback, got %s", urls[len(urls)-1])
	}
	if asn := geoURLs("GeoLite2-ASN.mmdb")[0]; !strings.HasSuffix(asn, "/GeoLite2-ASN.mmdb") {
		t.Fatalf("ASN asset case must be preserved, got %s", asn)
	}
}

func TestEnsureGeoDataSkipsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Country.mmdb")
	asset := geoAssetDefs["Country.mmdb"]
	content := bytes.Repeat([]byte("x"), int(asset.MinSize))
	content = append(content, []byte("\xab\xcd\xefMaxMind.com")...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Core{dataDir: dir}
	if err := c.ensureGeoData("rules:\n  - GEOIP,CN,DIRECT\n"); err != nil {
		t.Fatalf("ensureGeoData() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("existing geodata file should not be replaced")
	}
}

func TestEnsureGeoDataCopiesBundled(t *testing.T) {
	appDir := t.TempDir()
	dataDir := t.TempDir()
	geoDir := filepath.Join(appDir, "share", "geo")
	if err := os.MkdirAll(geoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	asset := geoAssetDefs["Country.mmdb"]
	content := bytes.Repeat([]byte("z"), int(asset.MinSize))
	content = append(content, []byte("\xab\xcd\xefMaxMind.com")...)
	if err := os.WriteFile(filepath.Join(geoDir, "Country.mmdb"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Core{appDir: appDir, dataDir: dataDir}
	if err := c.ensureGeoData("rules:\n  - GEOIP,CN,DIRECT\n"); err != nil {
		t.Fatalf("ensureGeoData() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dataDir, "Country.mmdb"))
	if err != nil {
		t.Fatalf("bundled geodata was not copied: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("copied geodata mismatch")
	}
}

func TestGeoAssetReadyRejectsBrokenMMDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Country.mmdb")
	asset := geoAssetDefs["Country.mmdb"]
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(asset.MinSize)), 0o600); err != nil {
		t.Fatal(err)
	}
	if geoAssetReady(path, asset) {
		t.Fatal("mmdb without MaxMind marker must be treated as invalid")
	}
}

func TestPurgeGeoData(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Country.mmdb", "Country.mmdb.download"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := &Core{dataDir: dir}
	c.purgeGeoData("rules:\n  - GEOIP,CN,DIRECT\n")
	for _, name := range []string{"Country.mmdb", "Country.mmdb.download"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should be removed", name)
		}
	}
}

func TestNeedsGeoRetry(t *testing.T) {
	cfg := "rules:\n  - GEOIP,CN,DIRECT\n"
	if !needsGeoRetry(errors.New("配置校验失败：can't initial MMDB"), cfg) {
		t.Fatal("mmdb error should trigger retry")
	}
	if needsGeoRetry(errors.New("配置校验失败：syntax error"), cfg) {
		t.Fatal("unrelated error should not trigger retry")
	}
	if needsGeoRetry(errors.New("MMDB invalid"), "rules:\n  - MATCH,DIRECT\n") {
		t.Fatal("config without geo rules should not trigger geodata retry")
	}
}

func TestFetchToFile(t *testing.T) {
	payload := strings.Repeat("y", 2<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "asset.download")
	if err := fetchToFile(context.Background(), srv.URL, dst, 1<<20); err != nil {
		t.Fatalf("fetchToFile() error = %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatal("downloaded content mismatch")
	}
}

func TestFetchToFileRejectsShortBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tiny"))
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "asset.download")
	if err := fetchToFile(context.Background(), srv.URL, dst, 1<<20); err == nil {
		t.Fatal("expected error for too-small download")
	}
}

func TestFetchToFileRejectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "asset.download")
	if err := fetchToFile(context.Background(), srv.URL, dst, 1); err == nil {
		t.Fatal("expected error for HTTP 502")
	}
}
