package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v2"

	"github.com/amber-store/oci-amber/browse"
)

// browseConfig is everything `browse` needs. browseConfigFromCLI fills it
// from flags; tests construct it directly and call runBrowse.
type browseConfig struct {
	Store   string
	LogFile string
	Start   string    // "", repo, repo:tag or repo@digest
	Stdout  io.Writer // nil means os.Stdout; must be a terminal
	Stderr  io.Writer // nil means os.Stderr
}

func browseFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "store", Usage: "store `directory` (required)", EnvVars: envVar("store"), Required: true},
		&cli.StringFlag{Name: "log-file", Usage: "write the log to `path`; without it warnings are printed after the screen closes", EnvVars: envVar("log-file")},
	}
}

func browseConfigFromCLI(c *cli.Context) (browseConfig, error) {
	if c.NArg() > 1 {
		return browseConfig{}, errors.New("browse takes at most one reference (repo, repo:tag or repo@digest)")
	}
	cfg := browseConfig{Store: c.String("store"), LogFile: c.String("log-file"), Start: c.Args().First()}
	if cfg.Store == "" {
		return browseConfig{}, errors.New("--store must not be empty")
	}
	return cfg, nil
}

// runBrowse checks that the store exists and stdout is a terminal, opens
// the store without touching the work directory and runs the browser
// until it quits. Nothing is written to the store: a missing store is an
// error rather than created, and the blob store is read-only.
func runBrowse(ctx context.Context, cfg browseConfig) error {
	stdout, stderr := cfg.Stdout, cfg.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	// openReadOnly checks this again; it is checked here first so a
	// missing store is reported before a missing terminal.
	if err := checkStoreExists(cfg.Store); err != nil {
		return err
	}
	if f, ok := stdout.(*os.File); !ok || !term.IsTerminal(f.Fd()) {
		return errors.New("browse needs a terminal")
	}

	// Logging: a file gets everything; otherwise warnings and errors are
	// kept for after the screen is gone, as the import TUI does.
	var deferred bytes.Buffer
	var logOut io.Writer = &deferred
	level := slog.LevelWarn
	if cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("opening log file: %w", err)
		}
		defer f.Close()
		logOut, level = f, slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level}))
	defer func() {
		if deferred.Len() > 0 {
			io.Copy(stderr, &deferred)
		}
	}()

	ro, err := openReadOnly(cfg.Store, log)
	if err != nil {
		return err
	}
	runErr := browse.Run(ctx, browse.Options{Store: ro.st, Blobs: ro.blobs, Images: ro.images, Start: cfg.Start})
	return errors.Join(runErr, ro.Close())
}
