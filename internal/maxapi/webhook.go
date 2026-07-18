package maxapi

import (
	"context"
	"encoding/json"
	"net/http"
)

type UpdateHandler func(context.Context, Update) error

func WebhookHandler(secret string, handle UpdateHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if secret != "" && r.Header.Get("X-Max-Bot-Api-Secret") != secret {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		defer r.Body.Close()

		var update Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := handle(r.Context(), update); err != nil {
			http.Error(w, "failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
