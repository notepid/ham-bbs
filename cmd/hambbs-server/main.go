// Command hambbs-server runs a HamBBS node.
//
//	hambbs-server                  start (runs the setup wizard on first start)
//	hambbs-server -init            run the setup wizard again
//	hambbs-server -set-admin-password
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/notepid/ham-bbs/internal/autorun"
	"github.com/notepid/ham-bbs/internal/bbs"
	"github.com/notepid/ham-bbs/internal/buildinfo"
	"github.com/notepid/ham-bbs/internal/config"
	"github.com/notepid/ham-bbs/internal/store"
	"github.com/notepid/ham-bbs/internal/transport/agwpe"
	"github.com/notepid/ham-bbs/internal/transport/tcp"
	"github.com/notepid/ham-bbs/internal/transport/vara"
	"github.com/notepid/ham-bbs/internal/web"
)

func main() {
	cfgPath := flag.String("config", "hambbs.toml", "configuration file")
	initCfg := flag.Bool("init", false, "run the setup wizard and write the configuration file")
	setPW := flag.Bool("set-admin-password", false, "set the admin web UI password")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("hambbs-server", buildinfo.String())
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(*cfgPath, *initCfg, *setPW, log); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func run(cfgPath string, initCfg, setPW bool, log *slog.Logger) error {
	_, statErr := os.Stat(cfgPath)
	if initCfg || errors.Is(statErr, fs.ErrNotExist) {
		if !interactive() {
			return fmt.Errorf("%s not found; run hambbs-server -init in a terminal to create it", cfgPath)
		}
		c, err := config.Wizard(os.Stdin, os.Stdout)
		if err != nil {
			return err
		}
		if err := config.SaveServer(cfgPath, c); err != nil {
			return err
		}
		abs, _ := filepath.Abs(cfgPath)
		fmt.Printf("\nSaved %s. Edit it any time; restart to apply.\n\n", abs)
	}

	cfg, err := config.LoadServer(cfgPath)
	if err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}

	if setPW {
		fmt.Print("New admin password: ")
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return err
		}
		if cfg.Admin.PasswordHash, err = config.HashPassword(strings.TrimSpace(string(pw))); err != nil {
			return err
		}
		if err := config.SaveServer(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Println("Admin password saved. Log in as user \"sysop\".")
		return nil
	}

	dbPath := cfg.DataPath("hambbs.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := bbs.New(cfg, st, log)
	srv.SetDialer("tcp", tcp.Dialer{})

	log.Info("HamBBS starting", "version", buildinfo.String(), "callsign", cfg.Callsign, "node", cfg.Node, "db", dbPath)
	if cfg.TCP.Enabled {
		l, err := tcp.Listen(cfg.TCP.Listen)
		if err != nil {
			return fmt.Errorf("tcp listen: %w", err)
		}
		defer l.Close()
		go func() {
			if err := srv.Serve(l); err != nil && ctx.Err() == nil {
				log.Error("tcp listener stopped", "err", err)
			}
		}()
	}

	// Helper programs (modem, TNC) started with the BBS. They stop after
	// the listeners have closed, as defers run in reverse order.
	var helpers []*autorun.Process
	startHelper := func(name, section, command string) error {
		if command == "" {
			return nil
		}
		p := autorun.New(name, log)
		if err := p.Start(command); err != nil {
			return fmt.Errorf("[%s] autorun: %w", section, err)
		}
		helpers = append(helpers, p)
		return nil
	}
	defer func() {
		for _, p := range helpers {
			p.Stop()
		}
	}()
	if cfg.VARA.Autorun != "" && !cfg.VARA.Enabled {
		log.Warn("[vara] autorun is set but vara is not enabled; not starting it")
	}
	if cfg.AX25.Autorun != "" && !cfg.AX25.Enabled {
		log.Warn("[ax25] autorun is set but ax25 is not enabled; not starting it")
	}

	if cfg.VARA.Enabled {
		if err := startHelper("vara modem", "vara", cfg.VARA.Autorun); err != nil {
			return err
		}
		l, err := vara.Listen(vara.Config{Addr: cfg.VARA.Addr, MyCalls: cfg.VARA.MyCalls, Log: log})
		if err != nil {
			return err
		}
		defer l.Close()
		srv.SetDialer("vara", l)
		go func() {
			if err := srv.Serve(l); err != nil && ctx.Err() == nil {
				log.Error("vara listener stopped", "err", err)
			}
		}()
	}

	if cfg.AX25.Enabled {
		if err := startHelper("ax25 TNC", "ax25", cfg.AX25.Autorun); err != nil {
			return err
		}
		l, err := agwpe.Listen(agwpe.Config{Addr: cfg.AX25.Addr, Port: cfg.AX25.Port, MyCalls: cfg.AX25.MyCalls, Log: log})
		if err != nil {
			return err
		}
		defer l.Close()
		srv.SetDialer("ax25", l)
		go func() {
			if err := srv.Serve(l); err != nil && ctx.Err() == nil {
				log.Error("ax25 listener stopped", "err", err)
			}
		}()
	}

	for _, nb := range cfg.Neighbors {
		switch kind, _ := nb.ConnectKind(); {
		case kind == "vara" && !cfg.VARA.Enabled, kind == "ax25" && !cfg.AX25.Enabled:
			log.Warn("neighbor uses a transport that is not enabled", "node", nb.Node, "connect", nb.Connect)
		}
	}
	go srv.Run(ctx) // scheduled forwarding needs the dialers set above

	if cfg.Admin.Listen != "" {
		if cfg.Admin.PasswordHash == "" && !isLoopback(cfg.Admin.Listen) {
			log.Warn("admin UI is reachable from the network without a password; run hambbs-server -set-admin-password")
		}
		hs := &http.Server{Addr: cfg.Admin.Listen, Handler: web.NewAdmin(srv, log, helpers...), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Info("admin UI", "url", "http://"+displayAddr(cfg.Admin.Listen)+"/")
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("admin UI stopped", "err", err)
			}
		}()
		defer hs.Shutdown(context.Background())
	}

	<-ctx.Done()
	log.Info("shutting down")
	return nil
}

func isLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		host = addr[:i]
	}
	return host == "127.0.0.1" || host == "localhost" || host == "[::1]"
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}
