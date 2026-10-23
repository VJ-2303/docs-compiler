package api

import "net/http"

func (app *Application) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/healthz", app.healthcheckHandler)
	mux.HandleFunc("POST /v1/documents", app.createDocumentHandler)
	mux.HandleFunc("GET /v1/documents/{id}", app.getDocumentHandler)

	return app.recoverPanic(app.logRequest(mux))
}
