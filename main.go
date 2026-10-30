package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

//go:embed index.html
var indexHTML []byte

type Slot struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Start    string `json:"start"`
	End      string `json:"end"`
}

type ResetRequest struct {
	Slots []Slot `json:"slots"`
}

type SlotWithAvailability struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Start     string `json:"start"`
	End       string `json:"end"`
	Available bool   `json:"available"`
}

type SlotsResponse struct {
	Slots []SlotWithAvailability `json:"slots"`
}

type NewBooking struct {
	SlotID string `json:"slot_id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
}

type Booking struct {
	ID        string `json:"id"`
	SlotID    string `json:"slot_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

type BookingsPage struct {
	Bookings   []Booking `json:"bookings"`
	NextCursor *string   `json:"next_cursor"`
}

const bookingCols = "id, slot_id, name, email, status, created_at"

var emailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

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

func errBody(msg string) map[string]string {
	return map[string]string{"error": msg}
}

func fmtTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func scanBooking(row pgx.Row) (Booking, error) {
	var b Booking
	var created time.Time
	err := row.Scan(&b.ID, &b.SlotID, &b.Name, &b.Email, &b.Status, &created)
	b.CreatedAt = fmtTime(created)
	return b, err
}

//  --------------- level 2: API key ----------------

func requireKey(key string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid API Key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ------------- level 5: rate limit (5 attempts per email per minute) ------------

type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

var rl = &limiter{hits: map[string][]time.Time{}}

func (l *limiter) allow(key string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-time.Minute)
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 5 {
		l.hits[key] = recent
		wait := int(math.Ceil(recent[0].Add(time.Minute).Sub(now).Seconds()))
		if wait < 1 {
			wait = 1
		}
		return false, wait
	}
	l.hits[key] = append(recent, now)
	return true, 0
}

func (l *limiter) reset() {
	l.mu.Lock()
	l.hits = map[string][]time.Time{}
	l.mu.Unlock()
}

// ----------- POST /reset -------------

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

		for _, q := range []string{
			"DELETE FROM idempotency_keys",
			"DELETE FROM bookings",
			"DELETE FROM slots",
		} {
			if _, err := tx.Exec(r.Context(), q); err != nil {
				writeError(w, http.StatusInternalServerError, "database error")
				return
			}
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

			_, err = tx.Exec(r.Context(),
				"INSERT INTO slots (id, provider,start_time, end_time) VALUES ($1, $2, $3, $4)",
				s.ID, s.Provider, start, end,
			)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "database error")
				return
			}
		}

		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		rl.reset()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ----------- GET /slots -------------

func handleListSlots(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provider := r.URL.Query().Get("provider")

		rows, err := pool.Query(r.Context(), `
			SELECT s.id, s.provider, s.start_time, s.end_time,
				NOT EXISTS (
				SELECT 1 FROM bookings b
				wHERE b.slot_id = s.id AND b.status = 'confirmed'
				) As available
			FROM slots s
			WHERE ($1::text = '' OR s.provider = $1)
			ORDER BY s.start_time
		`, provider)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()

		slots := []SlotWithAvailability{}
		for rows.Next() {
			var id, prov string
			var start, end time.Time
			var available bool
			if err := rows.Scan(&id, &prov, &start, &end, &available); err != nil {
				writeError(w, http.StatusInternalServerError, "database error")
				return
			}

			slots = append(slots, SlotWithAvailability{
				ID:        id,
				Provider:  prov,
				Start:     start.UTC().Format(time.RFC3339),
				End:       end.UTC().Format(time.RFC3339),
				Available: available,
			})
		}

		if err := rows.Err(); err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		writeJSON(w, http.StatusOK, SlotsResponse{Slots: slots})
	}
}

// ---------- POST /bookings -----------

func handleCreateBooking(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req NewBooking

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		req.SlotID = strings.TrimSpace(req.SlotID)
		req.Name = strings.TrimSpace(req.Name)
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))

		// level 5 every attempt counts, including ones that fail later

		if req.Email != "" {
			if ok, wait := rl.allow(req.Email); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(wait))
				writeError(w, http.StatusTooManyRequests, "too many booking attempts, try again later")
				return
			}
		}

		// level 2: validation
		if req.SlotID == "" || req.Name == "" || req.Email == "" || !emailRe.MatchString(req.Email) {
			writeError(w, http.StatusBadRequest, "slot_id, name and a valid email are required")
			return
		}

		ctx := r.Context()
		idemKey := r.Header.Get("Idempotency-Key")
		sum := sha256.Sum256([]byte(req.SlotID + "\x00" + req.Name + "\x00" + req.Email))
		hash := hex.EncodeToString(sum[:])

		tx, err := pool.Begin(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer tx.Rollback(ctx)

		// level 4: claim the key; a concurrent request with the same key waits here
		if idemKey != "" {
			tag, err := tx.Exec(ctx,
				`INSERT INTO idempotency_keys (key, request_hash) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`,
				idemKey, hash)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "database error")
				return
			}
			if tag.RowsAffected() == 0 {
				var storedHash, body string
				var code int
				err := tx.QueryRow(ctx,
					`SELECT request_hash, status_code, response_body FROM idempotency_keys WHERE key = $1`,
					idemKey).Scan(&storedHash, &code, &body)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "database error")
					return
				}
				if storedHash != hash {
					writeError(w, http.StatusUnprocessableEntity, "Idempotency-Key was already used with a different request body")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				w.Write([]byte(body))
				return
			}
		}

		// store the outcome under the key (if any), commit, and reply
		finish := func(status int, payload any) {
			body, _ := json.Marshal(payload)
			if idemKey != "" {
				if _, err := tx.Exec(ctx,
					`UPDATE idempotency_keys SET status_code = $1, response_body = $2 WHERE key = $3`,
					status, string(body), idemKey); err != nil {
					writeError(w, http.StatusInternalServerError, "database error")
					return
				}
			}
			if err := tx.Commit(ctx); err != nil {
				writeError(w, http.StatusInternalServerError, "database error")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write(body)
		}

		var start time.Time
		err = tx.QueryRow(ctx, `SELECT start_time FROM slots WHERE id = $1`, req.SlotID).Scan(&start)
		if errors.Is(err, pgx.ErrNoRows) {
			finish(http.StatusNotFound, errBody("slot not found"))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		if !start.After(time.Now()) {
			finish(http.StatusBadRequest, errBody("slot has already started"))
			return
		}

		// level 3: the unique index on confirmed bookings decides who wins
		b, err := scanBooking(tx.QueryRow(ctx, `
			INSERT INTO bookings (id, slot_id, name, email)
			VALUES (gen_random_uuid()::text, $1, $2, $3)
			ON CONFLICT (slot_id) WHERE status = 'confirmed' DO NOTHING
			RETURNING `+bookingCols, req.SlotID, req.Name, req.Email))
		if errors.Is(err, pgx.ErrNoRows) {
			finish(http.StatusConflict, errBody("slot already booked"))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		finish(http.StatusCreated, b)

		/*
			var b Booking
			var created time.Time

			err := pool.QueryRow(
				r.Context(),
				`
				INSERT INTO bookings (id, slot_id, name, email)
				VALUES (gen_random_uuid()::text, $1, $2, $3)
				RETURNING id, slot_id, name, email, status, created_at
				`,
				req.SlotID, req.Name, req.Email,
			).Scan(
				&b.ID,
				&b.SlotID,
				&b.Name,
				&b.Email,
				&b.Status,
				&created,
			)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "database error")
				return
			}

			b.CreatedAt = created.UTC().Format(time.RFC3339)
			writeJSON(w, http.StatusCreated, b)
		*/
	}
}

func handleGetBooking(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := scanBooking(pool.QueryRow(r.Context(),
			`SELECT `+bookingCols+` FROM bookings WHERE id = $1`, r.PathValue("id")))
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "booking not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		writeJSON(w, http.StatusOK, b)
	}
}

func handleCancelBooking(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")

		b, err := scanBooking(pool.QueryRow(ctx, `
			UPDATE bookings SET status = 'cancelled'
			WHERE id = $1 AND status = 'confirmed'
			RETURNING `+bookingCols, id))
		if err == nil {
			writeJSON(w, http.StatusOK, b)
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM bookings WHERE id = $1)`, id).Scan(&exists); err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		if exists {
			writeError(w, http.StatusConflict, "booking already cancelled")
		} else {
			writeError(w, http.StatusNotFound, "booking not found")
		}
	}
}

// ---------- GET /bookings?email=&limit=&cursor= (level 4) ----------

func encodeCursor(t time.Time, id string) string {
	raw := strconv.FormatInt(t.UnixMicro(), 10) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", errors.New("bad cursor")
	}
	micros, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", err
	}
	return time.UnixMicro(micros).UTC(), parts[1], nil
}

func handleListBookings(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		email := strings.ToLower(strings.TrimSpace(q.Get("email")))
		if email == "" {
			writeError(w, http.StatusBadRequest, "email is required")
			return
		}

		limit := 10
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 50 {
				writeError(w, http.StatusBadRequest, "limit must be a number from 1 to 50")
				return
			}
			limit = n
		}

		hasCursor := false
		var curTime time.Time
		var curID string
		if c := q.Get("cursor"); c != "" {
			t, id, err := decodeCursor(c)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid cursor")
				return
			}
			hasCursor, curTime, curID = true, t, id
		}

		rows, err := pool.Query(r.Context(), `
			SELECT `+bookingCols+` FROM bookings
			WHERE email = $1
			  AND ($2::boolean = false OR (created_at, id) > ($3::timestamptz, $4::text))
			ORDER BY created_at, id
			LIMIT $5`, email, hasCursor, curTime, curID, limit+1)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()

		items := []Booking{}
		times := []time.Time{}
		for rows.Next() {
			var b Booking
			var created time.Time
			if err := rows.Scan(&b.ID, &b.SlotID, &b.Name, &b.Email, &b.Status, &created); err != nil {
				writeError(w, http.StatusInternalServerError, "database error")
				return
			}
			b.CreatedAt = fmtTime(created)
			items = append(items, b)
			times = append(times, created)
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		page := BookingsPage{Bookings: items}
		if len(items) > limit {
			page.Bookings = items[:limit]
			c := encodeCursor(times[limit-1], items[limit-1].ID)
			page.NextCursor = &c
		}
		writeJSON(w, http.StatusOK, page)
	}
}

/*
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
	mux.HandleFunc("GET /slots", handleListSlots(pool))
	mux.HandleFunc("POST /bookings", handleCreateBooking(pool))

	log.Fatal(http.ListenAndServe(":8080", mux))
}
*/

func main() {
	_ = godotenv.Load() // on Render the variables come from the environment

	apiKey := os.Getenv("API_KEY")
	if apiKey == "" {
		log.Fatal("API_KEY is not set")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxConns = 10

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	if err := pool.Ping(context.Background()); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("POST /reset", handleReset(pool))
	mux.HandleFunc("GET /slots", handleListSlots(pool))
	mux.HandleFunc("POST /bookings", handleCreateBooking(pool))
	mux.HandleFunc("GET /bookings", handleListBookings(pool))
	mux.HandleFunc("GET /bookings/{id}", handleGetBooking(pool))
	mux.HandleFunc("DELETE /bookings/{id}", handleCancelBooking(pool))

	log.Println("listening on :" + port)
	log.Fatal(http.ListenAndServe(":"+port, requireKey(apiKey, mux)))
}
