package main

import (
	"context"
	"log"
	"net/http"

	registry "sherpa/internal/registry"
	"sherpa/internal/registry/api"
	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
)

type Config = registry.Config

func LoadConfig() (Config, error) {
	return registry.LoadConfig()
}

func run(ctx context.Context, cfg Config) (http.Handler, func(), error) {
	st, err := store.OpenPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		if err := st.Close(); err != nil {
			log.Printf("close registry store: %v", err)
		}
	}

	cs := content.NewBareGit(cfg.ContentDir)
	return api.New(st, cs, cfg.Token), cleanup, nil
}

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatal(err)
	}

	handler, cleanup, err := run(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()

	if err := http.ListenAndServe(":"+cfg.Port, handler); err != nil {
		log.Fatal(err)
	}
}
