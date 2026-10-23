package api

import (
	"log/slog"

	"github.com/vj/docx/internal/broker"
	"github.com/vj/docx/internal/store"
)

type Config struct {
	Port    int
	Env     string
	NatsURL string
}

type Application struct {
	Config    Config
	Store     *store.DocumentStore
	Logger    *slog.Logger
	Publisher *broker.Publisher
}

func New(cfg Config, store *store.DocumentStore, logger *slog.Logger, pub *broker.Publisher) *Application {
	return &Application{
		Config:    cfg,
		Store:     store,
		Logger:    logger,
		Publisher: pub,
	}
}
