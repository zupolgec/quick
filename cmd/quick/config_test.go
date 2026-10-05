package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// isolate points the user config dir at a temp folder (macOS uses HOME,
// Linux XDG_CONFIG_HOME) and clears the server env.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("QUICK_SERVER", "")
	return filepath.Dir(configPath())
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fakeJWT(aud string) string {
	p, _ := json.Marshal(map[string]any{"aud": aud, "email": "a@b.it"})
	return "e30." + base64.RawURLEncoding.EncodeToString(p) + ".sig"
}

func TestNormalizeServer(t *testing.T) {
	for in, want := range map[string]string{
		"quick.way.srl":             "https://quick.way.srl",
		"https://quick.way.srl/":    "https://quick.way.srl",
		" HTTPS://Quick.Way.SRL ":   "https://quick.way.srl",
		"http://127.0.0.1:8080//":   "http://127.0.0.1:8080",
		"https://quick.16bit.cloud": "https://quick.16bit.cloud",
	} {
		if got := normalizeServer(in); got != want {
			t.Errorf("normalizeServer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLegacyConfigAndTokenMigrate(t *testing.T) {
	dir := isolate(t)
	writeJSON(t, filepath.Join(dir, "config.json"), map[string]string{
		"server": "https://quick.way.srl", "oauth_client_id": "way-client",
		"hosted_domain": "wayexperience.it", "base_domain": "quick.way.srl",
	})
	writeJSON(t, filepath.Join(dir, "token.json"), tokenSet{
		IDToken: fakeJWT("way-client"), RefreshToken: "r1", Expiry: time.Now().Add(time.Hour),
	})

	cfg, err := resolveConfig("", "")
	if err != nil || cfg.Server != "https://quick.way.srl" || cfg.OAuthClientID != "way-client" {
		t.Fatalf("legacy config not used as default: %+v %v", cfg, err)
	}
	if !haveLogin(cfg.Server) {
		t.Fatal("legacy token not migrated to its server")
	}
	if _, ok := silentToken(cfg); !ok {
		t.Fatal("legacy token not usable without a new login")
	}
}

func TestTokensArePerServer(t *testing.T) {
	isolate(t)
	way := &cliConfig{Server: "https://quick.way.srl", OAuthClientID: "way-client"}
	bit := &cliConfig{Server: "https://quick.16bit.cloud", OAuthClientID: "bit-client"}
	saveServerConfig(way, true)
	saveServerConfig(bit, false)
	saveToken(way.Server, &tokenSet{IDToken: fakeJWT("way-client"), Expiry: time.Now().Add(time.Hour)})

	if _, ok := silentToken(bit); ok {
		t.Fatal("way token handed to the 16bit server")
	}
	saveToken(bit.Server, &tokenSet{IDToken: fakeJWT("bit-client"), Expiry: time.Now().Add(time.Hour)})
	if _, ok := silentToken(way); !ok {
		t.Fatal("logging in to 16bit dropped the way login")
	}
	if tok, ok := silentToken(bit); !ok || tok != fakeJWT("bit-client") {
		t.Fatal("16bit token not returned for 16bit")
	}
}

func TestTokenForOtherClientIsIgnored(t *testing.T) {
	isolate(t)
	cfg := &cliConfig{Server: "https://quick.way.srl", OAuthClientID: "new-client"}
	saveServerConfig(cfg, true)
	saveToken(cfg.Server, &tokenSet{IDToken: fakeJWT("old-client"), Expiry: time.Now().Add(time.Hour)})
	if _, ok := silentToken(cfg); ok {
		t.Fatal("token issued for another client accepted")
	}
}

func TestServerPrecedence(t *testing.T) {
	isolate(t)
	saveServerConfig(&cliConfig{Server: "https://def.example", OAuthClientID: "d"}, true)
	saveServerConfig(&cliConfig{Server: "https://site.example", OAuthClientID: "s"}, false)
	saveServerConfig(&cliConfig{Server: "https://env.example", OAuthClientID: "e"}, false)
	saveServerConfig(&cliConfig{Server: "https://flag.example", OAuthClientID: "f"}, false)

	check := func(flag, site, want string) {
		t.Helper()
		cfg, err := resolveConfig(flag, site)
		if err != nil || cfg.Server != want {
			t.Errorf("resolveConfig(%q, %q) = %v %v, want %s", flag, site, cfg, err, want)
		}
	}
	check("", "", "https://def.example")
	check("", "site.example", "https://site.example")
	t.Setenv("QUICK_SERVER", "env.example/")
	check("", "site.example", "https://env.example")
	check("flag.example", "site.example", "https://flag.example")
}

func TestNewServerDoesNotStealDefault(t *testing.T) {
	isolate(t)
	saveServerConfig(&cliConfig{Server: "https://quick.way.srl", OAuthClientID: "w"}, true)
	saveServerConfig(&cliConfig{Server: "https://quick.16bit.cloud", OAuthClientID: "b"}, false)
	cfg, _ := resolveConfig("", "")
	if cfg.Server != "https://quick.way.srl" {
		t.Fatalf("default changed to %s", cfg.Server)
	}
	// old binaries still read the top-level fields: they must mirror the default.
	b, _ := os.ReadFile(configPath())
	var legacy cliConfig
	json.Unmarshal(b, &legacy)
	if legacy.Server != "https://quick.way.srl" || legacy.OAuthClientID != "w" {
		t.Fatalf("legacy mirror broken: %+v", legacy)
	}
}
