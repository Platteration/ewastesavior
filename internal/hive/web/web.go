// Package web serves the hive's embedded dashboard.
package web

import (
	"io"
	"net/http"
)

// Handler serves the dashboard. Placeholder until the dashboard is implemented.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "SaviorOS hive dashboard: not built yet\n")
	})
}
