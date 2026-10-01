package main

import (
	"flag"
	"testing"
)

func TestParseConfig(t *testing.T) {
	env := map[string]string{"VINX_DATA": "/env/data"}
	getenv := func(k string) string { return env[k] }

	cfg, err := parseConfig(flag.NewFlagSet("serve", flag.ContinueOnError), nil, getenv, "/home/u", true)
	if err != nil || cfg.DataDir != "/env/data" || cfg.Listen != "0.0.0.0:3100" {
		t.Fatalf("env defaults: %+v %v", cfg, err)
	}
	cfg, err = parseConfig(flag.NewFlagSet("serve", flag.ContinueOnError), []string{"--data", "/flag", "--listen", "127.0.0.1:9"}, getenv, "/home/u", true)
	if err != nil || cfg.DataDir != "/flag" || cfg.Listen != "127.0.0.1:9" {
		t.Fatalf("flags win: %+v %v", cfg, err)
	}
	cfg, _ = parseConfig(flag.NewFlagSet("x", flag.ContinueOnError), nil, func(string) string { return "" }, "/home/u", false)
	if cfg.DataDir != "/home/u/.local/share/vinx-assistant" {
		t.Fatalf("home default: %+v", cfg)
	}
}

func TestParseConfigBasePath(t *testing.T) {
	none := func(string) string { return "" }
	for in, want := range map[string]string{"": "", "/": "", "todo": "/todo", "/todo/": "/todo", "/a/b": "/a/b"} {
		cfg, err := parseConfig(flag.NewFlagSet("serve", flag.ContinueOnError), []string{"--base-path", in}, none, "/h", true)
		if err != nil || cfg.BasePath != want {
			t.Fatalf("%q: %q %v", in, cfg.BasePath, err)
		}
	}
	cfg, err := parseConfig(flag.NewFlagSet("serve", flag.ContinueOnError), nil, func(k string) string {
		if k == "VINX_BASE_PATH" {
			return "/env/"
		}
		return ""
	}, "/h", true)
	if err != nil || cfg.BasePath != "/env" {
		t.Fatalf("env: %q %v", cfg.BasePath, err)
	}
	for _, bad := range []string{"/a?b", "/a#b", "/a b", "/../x", "/a/..", "/a//b"} {
		if _, err := parseConfig(flag.NewFlagSet("serve", flag.ContinueOnError), []string{"--base-path", bad}, none, "/h", true); err == nil {
			t.Fatalf("%q 应报错", bad)
		}
	}
}
