// Command noters runs the local task server behind the noter board.
//
// The UI is hosted on GitHub Pages and dials this process on loopback; if it
// is not running, the page shows instructions for starting it.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alexbathome/noter/internal/store"
	"github.com/alexbathome/noter/internal/web"
)

// The same static UI GitHub Pages serves, embedded so the binary works offline.
//
//go:embed all:docs
var docsFS embed.FS

const defaultPort = 11911

func main() {
	log.SetFlags(0)
	log.SetPrefix("noters: ")

	if len(os.Args) < 2 || os.Args[1] != "web" {
		fmt.Fprintf(os.Stderr, "usage: noters web [--port %d] [--db PATH]\n", defaultPort)
		os.Exit(2)
	}

	fs := flag.NewFlagSet("web", flag.ExitOnError)
	port := fs.Int("port", defaultPort, "loopback port to listen on")
	dbPath := fs.String("db", defaultDBPath(), "path to the bolt database")
	fs.Parse(os.Args[2:])

	if err := run(*port, *dbPath); err != nil {
		log.Fatal(err)
	}
}

func run(port int, dbPath string) error {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	static, err := fs.Sub(docsFS, "docs")
	if err != nil {
		return err
	}

	// Loopback only: this server is a personal datastore and must never be
	// reachable from the network.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	base := fmt.Sprintf("http://localhost:%d", port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           web.New(db, base, static).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Printf("listening on %s (db: %s)", base, dbPath)
	log.Printf("open the board at %s or at your GitHub Pages URL", base)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// defaultDBPath keeps the database in the user's config dir so the board
// survives running the binary from anywhere via `go run ...@latest`.
func defaultDBPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "noters.db"
	}
	return filepath.Join(dir, "noters", "noters.db")
}
