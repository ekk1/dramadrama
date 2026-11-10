// Command dramadrama runs the short-drama production workbench.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/pipeline"
	"github.com/ekk1/dramadrama/internal/project"
	"github.com/ekk1/dramadrama/internal/reqlog"
	"github.com/ekk1/dramadrama/internal/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	dataDir := flag.String("data", "data", "data directory (projects, assets, prompt overrides, request log)")
	cfgPath := flag.String("config", "", "config file (default <data>/config.json)")
	devDir := flag.String("dev", "", "serve templates/static from this directory (e.g. ./web) and reload on each request")
	flag.Parse()

	if err := run(*addr, *dataDir, *cfgPath, *devDir); err != nil {
		fmt.Fprintln(os.Stderr, "dramadrama:", err)
		os.Exit(1)
	}
}

func run(addr, dataDir, cfgPath, devDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	if cfgPath == "" {
		cfgPath = filepath.Join(dataDir, "config.json")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	projects, err := project.Open(filepath.Join(dataDir, "projects"))
	if err != nil {
		return err
	}
	lib, err := assets.Open(filepath.Join(dataDir, "assets"))
	if err != nil {
		return err
	}
	prompts := &pipeline.Prompts{OverrideDir: filepath.Join(dataDir, "prompts")}
	logs, err := reqlog.Open(filepath.Join(dataDir, "logs"))
	if err != nil {
		return err
	}
	pipe := pipeline.New(cfg, projects, lib, prompts)
	pipe.Log = logs

	h, err := server.New(server.Options{
		Config: cfg, Projects: projects, Assets: lib, Pipeline: pipe, Prompts: prompts, Logs: logs, DevDir: devDir,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           logRequests(h),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: step runs stream their output for minutes.
	}
	log.Printf("dramadrama listening on http://%s (data: %s, config: %s)", addr, dataDir, cfgPath)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		if r.Method != http.MethodGet {
			log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}
