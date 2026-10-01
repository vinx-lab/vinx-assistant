package main

import (
	"flag"
	"path/filepath"

	"github.com/vinx-lab/vinx-assistant/internal/app"
)

func parseConfig(fs *flag.FlagSet, args []string, getenv func(string) string, home string, withListen bool) (app.Config, error) {
	def := func(env, fallback string) string {
		if v := getenv(env); v != "" {
			return v
		}
		return fallback
	}
	cfg := app.Config{}
	fs.StringVar(&cfg.DataDir, "data", def("VINX_DATA", filepath.Join(home, ".local/share/vinx-assistant")), "数据目录")
	if withListen {
		fs.StringVar(&cfg.Listen, "listen", def("VINX_LISTEN", "0.0.0.0:3100"), "网页监听地址")
	}
	err := fs.Parse(args)
	return cfg, err
}
