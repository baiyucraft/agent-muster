package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/update"
)

func main() {
	if handled, err := update.HandleHelper(os.Args[1:]); handled {
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	profile := flag.String("profile", "test", "")
	manifest := flag.String("manifest", "", "")
	health := flag.String("update-health", "", "")
	flag.Parse()

	unlock, err := update.LockInstallation(*health)
	if err != nil {
		return err
	}
	defer unlock()

	if *manifest != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		offer, err := update.FetchOffer(ctx, *manifest)
		if err != nil {
			return err
		}
		key, err := update.LoadPublicKey()
		if err != nil {
			return err
		}
		prepared, err := update.Stage(ctx, offer, key)
		if err != nil {
			return err
		}
		defer prepared.Discard()
		return prepared.Start(*profile, "upd-0123456789abcdef0123456789abcdef")
	}

	if err := update.MarkHealthy(*health); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	root := filepath.Dir(exe)
	if err := os.WriteFile(filepath.Join(root, "started"), []byte(update.Version), 0600); err != nil {
		return err
	}
	quit := filepath.Join(root, "stop")
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(quit); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("test did not request device shutdown")
}
