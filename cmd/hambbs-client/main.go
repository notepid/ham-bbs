// Command hambbs-client runs the HamBBS client: a local mailbox with a web
// UI in your browser.
//
//	hambbs-client              start and open the browser
//	hambbs-client -no-browser  start without opening the browser
//	hambbs-client -sync        sync once and exit (for scripts/cron)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/notepid/ham-bbs/internal/buildinfo"
	"github.com/notepid/ham-bbs/internal/client"
	"github.com/notepid/ham-bbs/internal/config"
	"github.com/notepid/ham-bbs/internal/store"
	"github.com/notepid/ham-bbs/internal/web"
)

func main() {
	cfgPath := flag.String("config", "hambbs-client.toml", "configuration file")
	noBrowser := flag.Bool("no-browser", false, "do not open the browser")
	syncOnce := flag.Bool("sync", false, "sync once and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("hambbs-client", buildinfo.String())
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(*cfgPath, *noBrowser, *syncOnce, log); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(cfgPath string, noBrowser, syncOnce bool, log *slog.Logger) error {
	cfg, err := config.LoadClient(cfgPath)
	if errors.Is(err, fs.ErrNotExist) {
		if err := config.SaveClient(cfg); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}

	dbPath := cfg.DataPath("mailbox.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open mailbox: %w", err)
	}
	defer st.Close()
	c := client.New(cfg, st)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if syncOnce {
		rep := c.Sync(ctx)
		if rep.Err != nil {
			return rep.Err
		}
		fmt.Printf("Synced with %s in %s: %d sent, %d messages and %d headers received (%d bytes out, %d in).\n",
			rep.Server, rep.Duration.Round(time.Millisecond), rep.Uploaded, rep.NewBodies, rep.NewHeaders, rep.BytesOut, rep.BytesIn)
		for _, rj := range rep.Rejected {
			fmt.Printf("Rejected %s: %s\n", rj.ID, rj.Reason)
		}
		return nil
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w (is the client already running?)", cfg.Listen, err)
	}
	url := "http://" + ln.Addr().String() + "/"
	hs := &http.Server{Handler: web.NewClientUI(c, log), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("web UI stopped", "err", err)
			stop()
		}
	}()
	log.Info("HamBBS client running", "version", buildinfo.String(), "url", url, "config", cfgPath)
	fmt.Printf("\nHamBBS client is running. Open %s in your browser.\nPress Ctrl+C to stop.\n\n", url)
	if !noBrowser {
		if err := web.OpenBrowser(url); err != nil {
			log.Warn("could not open browser", "err", err)
		}
	}

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(shutdownCtx)
}
