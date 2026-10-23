package api

import (
	"fmt"
	"net/http"
	"time"
)

type statusResponseWriter struct {
	http.ResponseWriter
	statuscode int
}

func (w *statusResponseWriter) WriteHeader(code int) {
	w.statuscode = code
	w.ResponseWriter.WriteHeader(code)
}

func (app *Application) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if pv := recover(); pv != nil {
				w.Header().Set("Connection", "close")
				app.serverErrorResponse(w, r, fmt.Errorf("%v", pv))
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func (app *Application) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		srw := &statusResponseWriter{ResponseWriter: w, statuscode: http.StatusOK}

		next.ServeHTTP(srw, r)

		app.Logger.Info(
			"request processed",
			"method", r.Method,
			"uri", r.URL.RequestURI(),
			"status", srw.statuscode,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
