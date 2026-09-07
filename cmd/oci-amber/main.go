// Command oci-amber runs an OCI distribution registry whose storage is an
// embedded amber store. The `serve` subcommand runs the registry, `import`
// stores a `docker image save` archive without running it, `save` writes
// one, `ls` lists a store's images and `browse` walks a store in the
// terminal.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/registry"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
	"github.com/urfave/cli/v2"
)

const (
	// readHeaderTimeout bounds how long a client may take to send request
	// headers. Bodies (blob uploads) are deliberately unbounded.
	readHeaderTimeout = 10 * time.Second
	// shutdownTimeout is how long graceful shutdown waits for in-flight
	// handlers before connections are cut and the store is closed anyway.
	shutdownTimeout = 30 * time.Second
	// envPrefix plus the upper-cased, underscored flag name is the environment
	// variable that can set a flag (--max-in-memory -> OCI_AMBER_MAX_IN_MEMORY).
	envPrefix = "OCI_AMBER_"
	// workSubdir is the directory the registry owns inside --work-dir. All
	// in-flight state lives under it, and it is the only thing under
	// --work-dir that is ever created or deleted, so --work-dir may be a
	// scratch directory (/var/tmp, say) the operator uses for other things
	// as well.
	workSubdir = "oci-amber"

	defaultListen             = ":5000"
	defaultMaxInMemory        = "64MiB"
	defaultVerifyLimit        = "150MiB"
	defaultAnalyzeParallelism = 2
	defaultAnalyzeTimeout     = 15 * time.Minute
	defaultUploadTimeout      = time.Hour
	defaultLogLevel           = "info"
)

// config is everything `serve` needs. configFromCLI fills it from flags;
// tests construct it directly and call run.
type config struct {
	Store                 string
	WorkDir               string // "" means <Store>/work
	Listen                string
	MaxInMemory           int64
	AnalyzeParallelism    int
	AnalyzeTimeout        time.Duration
	MaxConcurrentFinalize int
	VerifyLimit           int64
	VerifyRoundTrip       bool
	AllowRaw              bool
	UploadTimeout         time.Duration
	GCInterval            time.Duration
	LogLevel              slog.Level
	LogOutput             io.Writer           // nil means os.Stderr
	OnListen              func(addr net.Addr) // optional; called once the listener is bound
	// Stop, when set, is called the moment ctx.Done fires, before the
	// shutdown drain starts. main wires this to signal.NotifyContext's
	// cancel/stop function, so it also stops relaying SIGINT/SIGTERM to ctx:
	// a second signal during the (potentially long) drain then takes the
	// default action and terminates the process, instead of being silently
	// absorbed by a context that is already cancelled.
	Stop func()
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmds := commands{
		Serve: func(ctx context.Context, cfg config) error {
			cfg.Stop = stop
			return run(ctx, cfg)
		},
		Import: runImport,
		Browse: runBrowse,
		Ls:     runLs,
		Save:   runSave,
	}
	if err := newApp(cmds).RunContext(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "oci-amber:", err)
		os.Exit(1)
	}
}

// defaultMaxConcurrentFinalize is NumCPU/2 with a floor of 1.
func defaultMaxConcurrentFinalize() int {
	return max(1, runtime.NumCPU()/2)
}

// envVar returns the environment variable that sets the named flag.
func envVar(flag string) []string {
	return []string{envPrefix + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))}
}

// commands is what each subcommand runs once its flags have been
// validated. main wires the real functions; tests wire ones that capture
// the config. A nil entry is a no-op.
type commands struct {
	Serve  func(ctx context.Context, cfg config) error
	Import func(ctx context.Context, cfg importConfig) error
	Browse func(ctx context.Context, cfg browseConfig) error
	Ls     func(ctx context.Context, cfg lsConfig) error
	Save   func(ctx context.Context, cfg saveConfig) error
}

// newApp builds the command line application over cmds.
func newApp(cmds commands) *cli.App {
	serve, imp, brw := cmds.Serve, cmds.Import, cmds.Browse
	if serve == nil {
		serve = func(context.Context, config) error { return nil }
	}
	if imp == nil {
		imp = func(context.Context, importConfig) error { return nil }
	}
	if brw == nil {
		brw = func(context.Context, browseConfig) error { return nil }
	}
	ls := cmds.Ls
	if ls == nil {
		ls = func(context.Context, lsConfig) error { return nil }
	}
	save := cmds.Save
	if save == nil {
		save = func(context.Context, saveConfig) error { return nil }
	}
	return &cli.App{
		Name:            "oci-amber",
		Usage:           "OCI distribution registry backed by an embedded amber store",
		HideHelpCommand: true,
		Commands: []*cli.Command{{
			Name:  "serve",
			Usage: "run the registry",
			Flags: serveFlags(),
			Action: func(c *cli.Context) error {
				cfg, err := configFromCLI(c)
				if err != nil {
					return err
				}
				return serve(c.Context, cfg)
			},
		}, {
			Name:      "import",
			Usage:     "store a `docker image save` archive without running the registry",
			ArgsUsage: "<archive.tar | ->",
			Flags:     importFlags(),
			Action: func(c *cli.Context) error {
				cfg, err := importConfigFromCLI(c)
				if err != nil {
					return err
				}
				return imp(c.Context, cfg)
			},
		}, {
			Name:      "browse",
			Usage:     "browse a store in the terminal: images, how they are stored, their root filesystems",
			ArgsUsage: "[repo[:tag|@digest]]",
			Flags:     browseFlags(),
			Action: func(c *cli.Context) error {
				cfg, err := browseConfigFromCLI(c)
				if err != nil {
					return err
				}
				return brw(c.Context, cfg)
			},
		}, {
			Name:      "ls",
			Usage:     "list the images in a store",
			ArgsUsage: "[repo]",
			Flags:     lsFlags(),
			Action: func(c *cli.Context) error {
				cfg, err := lsConfigFromCLI(c)
				if err != nil {
					return err
				}
				return ls(c.Context, cfg)
			},
		}, {
			Name:      "save",
			Usage:     "write images to a `docker image save` archive",
			ArgsUsage: "<repo[:tag|@digest]>...",
			Flags:     saveFlags(),
			Action: func(c *cli.Context) error {
				cfg, err := saveConfigFromCLI(c)
				if err != nil {
					return err
				}
				return save(c.Context, cfg)
			},
		}},
	}
}

// storeFlags is the flag table `serve` and `import` share: everything
// about the store and the blob pipeline. Every flag can also be set
// through OCI_AMBER_<FLAG>.
func storeFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "store", Usage: "store `directory` (required)", EnvVars: envVar("store"), Required: true},
		&cli.StringFlag{Name: "work-dir", Usage: "`directory` holding <work-dir>/oci-amber/{uploads,spool}, whose contents are deleted at startup; nothing else under it is touched (default: <store>/work)", EnvVars: envVar("work-dir")},
		&cli.StringFlag{Name: "max-in-memory", Value: defaultMaxInMemory, Usage: "upload spool and zrecipe spool threshold (`size` with unit B, KiB, MiB, GiB, KB, MB or GB)", EnvVars: envVar("max-in-memory")},
		&cli.IntFlag{Name: "analyze-parallelism", Value: defaultAnalyzeParallelism, Usage: "zrecipe candidate `workers` per blob", EnvVars: envVar("analyze-parallelism")},
		&cli.DurationFlag{Name: "analyze-timeout", Value: defaultAnalyzeTimeout, Usage: "per-blob analyze `deadline`; on expiry the blob cannot be a prism (see --allow-raw)", EnvVars: envVar("analyze-timeout")},
		&cli.IntFlag{Name: "max-concurrent-finalize", Value: defaultMaxConcurrentFinalize(), Usage: "concurrent blob finalizations, and platform manifest publishes on import (`count`, default NumCPU/2, minimum 1)", EnvVars: envVar("max-concurrent-finalize")},
		&cli.StringFlag{Name: "verify-limit", Value: defaultVerifyLimit, Usage: "accept a layer's recompression once this much of its compressed `size` has been reproduced instead of recompressing all of it (same units as --max-in-memory); 0 verifies every byte", EnvVars: envVar("verify-limit")},
		&cli.BoolFlag{Name: "verify-roundtrip", Value: false, Usage: "diagnostic: also run the full pull pipeline over every prism before publishing it; a mismatch is a round-trip failure", EnvVars: envVar("verify-roundtrip")},
		&cli.BoolFlag{Name: "allow-raw", Usage: "store a layer raw when it cannot be stored as a prism, instead of refusing the upload; blobs that are not tars are always stored raw", EnvVars: envVar("allow-raw")},
		&cli.StringFlag{Name: "log-level", Value: defaultLogLevel, Usage: "log `level`: debug, info, warn or error", EnvVars: envVar("log-level")},
	}
}

// serveFlags is the flag table from the spec's Configuration section. Every
// flag can also be set through OCI_AMBER_<FLAG>.
func serveFlags() []cli.Flag {
	return append(storeFlags(),
		&cli.StringFlag{Name: "listen", Value: defaultListen, Usage: "listen `address`", EnvVars: envVar("listen")},
		&cli.DurationFlag{Name: "upload-timeout", Value: defaultUploadTimeout, Usage: "idle upload session expiry and recent-uploads table TTL (`duration`)", EnvVars: envVar("upload-timeout")},
		&cli.DurationFlag{Name: "gc-interval", Value: 0, Usage: "background GC cycle `interval`, 0 disables", EnvVars: envVar("gc-interval")},
	)
}

// configFromCLI reads and validates the serve flags.
func configFromCLI(c *cli.Context) (config, error) {
	cfg := config{
		Store:                 c.String("store"),
		WorkDir:               c.String("work-dir"),
		Listen:                c.String("listen"),
		AnalyzeParallelism:    c.Int("analyze-parallelism"),
		AnalyzeTimeout:        c.Duration("analyze-timeout"),
		MaxConcurrentFinalize: c.Int("max-concurrent-finalize"),
		VerifyRoundTrip:       c.Bool("verify-roundtrip"),
		AllowRaw:              c.Bool("allow-raw"),
		UploadTimeout:         c.Duration("upload-timeout"),
		GCInterval:            c.Duration("gc-interval"),
	}
	if cfg.Store == "" {
		return config{}, errors.New("--store must not be empty")
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(cfg.Store, "work")
	}
	if cfg.Listen == "" {
		return config{}, errors.New("--listen must not be empty")
	}
	size, err := parseSize(c.String("max-in-memory"))
	if err != nil {
		return config{}, fmt.Errorf("--max-in-memory: %w", err)
	}
	cfg.MaxInMemory = size
	limit, err := parseSize(c.String("verify-limit"))
	if err != nil {
		return config{}, fmt.Errorf("--verify-limit: %w", err)
	}
	cfg.VerifyLimit = limit
	if cfg.AnalyzeParallelism < 1 {
		return config{}, fmt.Errorf("--analyze-parallelism must be at least 1, got %d", cfg.AnalyzeParallelism)
	}
	if cfg.AnalyzeTimeout <= 0 {
		return config{}, fmt.Errorf("--analyze-timeout must be positive, got %s", cfg.AnalyzeTimeout)
	}
	if cfg.MaxConcurrentFinalize < 1 {
		return config{}, fmt.Errorf("--max-concurrent-finalize must be at least 1, got %d", cfg.MaxConcurrentFinalize)
	}
	if cfg.UploadTimeout <= 0 {
		return config{}, fmt.Errorf("--upload-timeout must be positive, got %s", cfg.UploadTimeout)
	}
	if cfg.GCInterval < 0 {
		return config{}, fmt.Errorf("--gc-interval must not be negative, got %s", cfg.GCInterval)
	}
	level, err := parseLogLevel(c.String("log-level"))
	if err != nil {
		return config{}, fmt.Errorf("--log-level: %w", err)
	}
	cfg.LogLevel = level
	return cfg, nil
}

// run opens the store, wires the packages together and serves until ctx is
// cancelled, then shuts down gracefully: stop accepting, wait up to
// shutdownTimeout for handlers, close the upload manager and the store. It
// returns nil after a clean shutdown.
func run(ctx context.Context, cfg config) error {
	if cfg.Store == "" {
		return errors.New("store directory is required")
	}
	if cfg.Listen == "" {
		return errors.New("listen address is required")
	}
	out := cfg.LogOutput
	if out == nil {
		out = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: cfg.LogLevel}))

	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = filepath.Join(cfg.Store, "work")
	}
	// Everything in flight lives under <work-dir>/oci-amber, which belongs
	// to the registry; nothing else under the operator's --work-dir is
	// created, read or deleted. The two owned subdirectories clean
	// themselves at startup: upload.NewManager empties uploads/ and
	// blob.New empties spool/, in both cases only the contents.
	ownDir := filepath.Join(workDir, workSubdir)
	uploadsDir := filepath.Join(ownDir, "uploads")
	if err := os.MkdirAll(ownDir, 0o755); err != nil {
		return fmt.Errorf("creating work directory %s: %w", ownDir, err)
	}

	st, err := store.Open(cfg.Store, store.Options{GCInterval: cfg.GCInterval, Logger: log})
	if err != nil {
		return fmt.Errorf("opening store %s: %w", cfg.Store, err)
	}

	blobs, err := blob.New(st, blob.Options{
		WorkDir:               ownDir,
		MaxInMemory:           cfg.MaxInMemory,
		AnalyzeParallelism:    cfg.AnalyzeParallelism,
		AnalyzeTimeout:        cfg.AnalyzeTimeout,
		MaxConcurrentFinalize: cfg.MaxConcurrentFinalize,
		VerifyLimit:           cfg.VerifyLimit,
		VerifyRoundTrip:       cfg.VerifyRoundTrip,
		AllowRaw:              cfg.AllowRaw,
		RecentTTL:             cfg.UploadTimeout,
	}, log)
	if err != nil {
		return errors.Join(fmt.Errorf("creating blob store: %w", err), st.Close())
	}

	uploads, err := upload.NewManager(uploadsDir, cfg.MaxInMemory, cfg.UploadTimeout, log)
	if err != nil {
		return errors.Join(fmt.Errorf("creating upload manager: %w", err), st.Close())
	}

	images := image.New(st, blobs, log)
	handler := registry.New(blobs, images, uploads, log)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.Join(fmt.Errorf("listening on %s: %w", cfg.Listen, err), uploads.Close(), st.Close())
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	log.Info("oci-amber listening",
		"addr", ln.Addr().String(),
		"store", cfg.Store,
		"work_dir", workDir,
		"max_in_memory", cfg.MaxInMemory,
		"analyze_parallelism", cfg.AnalyzeParallelism,
		"analyze_timeout", cfg.AnalyzeTimeout,
		"max_concurrent_finalize", cfg.MaxConcurrentFinalize,
		"verify_limit", cfg.VerifyLimit,
		"verify_roundtrip", cfg.VerifyRoundTrip,
		"allow_raw", cfg.AllowRaw,
		"upload_timeout", cfg.UploadTimeout,
		"gc_interval", cfg.GCInterval,
	)
	if cfg.OnListen != nil {
		cfg.OnListen(ln.Addr())
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	var errs []error
	select {
	case err := <-serveErr:
		// Serve only returns on its own when accepting fails; Shutdown has
		// not been called, so this is never ErrServerClosed.
		errs = append(errs, fmt.Errorf("serving on %s: %w", ln.Addr(), err))
	case <-ctx.Done():
		log.Info("shutting down", "reason", ctx.Err().Error(), "timeout", shutdownTimeout)
		// Stop relaying signals now, at the start of the drain, not after it
		// finishes: a second SIGINT/SIGTERM during a slow drain must be able
		// to kill the process outright rather than being swallowed by a
		// context that is already done.
		if cfg.Stop != nil {
			cfg.Stop()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("handlers still running after shutdown timeout; closing connections", "error", err)
			if err := srv.Close(); err != nil {
				log.Warn("closing server", "error", err)
			}
		}
		cancel()
		<-serveErr // http.ErrServerClosed, as soon as Shutdown/Close was called
	}

	if err := uploads.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing upload manager: %w", err))
	}
	if err := st.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing store: %w", err))
	}
	log.Info("oci-amber stopped")
	return errors.Join(errs...)
}
