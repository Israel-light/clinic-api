package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

type Slot struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Start    string `json:"start"`
	End      string `json:"end"`
}

type ResetRequest struct {
	Slots []Slot `json:"slots"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{
		"error": msg,
	})
}

func handleReset(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ResetRequest

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		tx, err := pool.Begin(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer tx.Rollback(r.Context())

		_, err = tx.Exec(r.Context(), "DELETE FROM bookings")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		_, err = tx.Exec(r.Context(), "DELETE FROM slots")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		for _, s := range req.Slots {
			start, err := time.Parse(time.RFC3339, s.Start)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid start time")
				return
			}

			end, err := time.Parse(time.RFC3339, s.End)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid end time")
				return
			}

			_, err = tx.Exec(
				r.Context(),
				"INSERT INTO slots (id, provider,start_time, end_time) VALUES ($1, $2, $3, $4)",
				s.ID, s.Provider, start, end,
			)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "database error")
				return
			}
		}

		err = tx.Commit(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func main() {
	// 1. Load environment variables from  .env
	if err := godotenv.Load(); err != nil {
		log.Fatal(err)
	}

	// 2. Read the database URL
	url := os.Getenv("DATABASE_URL")

	// 3. Connect to the database
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	// 4. Ping the database
	if err := pool.Ping(context.Background()); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status": "ok"}`)
	})

	mux.HandleFunc("POST /reset", handleReset(pool))

	log.Fatal(http.ListenAndServe(":8080", mux))
}
