package api

import (
	"log/slog"

	"github.com/vj/docx/internal/store"
)

type Config struct {
	Port int
	Env  string
}

type Application struct {
	Config Config
	Store  *store.DocumentStore
	Logger *slog.Logger
}

func New(cfg Config, store *store.DocumentStore, logger *slog.Logger) *Application {
	return &Application{
		Config: cfg,
		Store:  store,
		Logger: logger,
	}
}
