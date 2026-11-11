// Command triptracker serves a live journey map and the location API used by the phone app.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Invictus321/triptracker/internal/tracker"
	"github.com/Invictus321/triptracker/web"
)

func main() {
	defaultAddr := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		defaultAddr = ":" + port
	}
	addr := flag.String("addr", defaultAddr, "listen address")
	dataPath := flag.String("data", "data/journey.json", "file used to persist the current journey (empty to disable)")
	flag.Parse()

	token := os.Getenv("TRIPTRACKER_TOKEN")
	if token == "" {
		log.Print("warning: TRIPTRACKER_TOKEN is not set; anyone who can reach the server can post locations")
	}

	store, err := tracker.Open(*dataPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           tracker.NewServer(store, token, web.Static()).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("triptracker listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
