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
