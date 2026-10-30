# Clinic API

A Go-based appointment booking API for a clinic. It stores appointment slots in PostgreSQL, exposes booking endpoints with validation and idempotency protection, and serves a small embedded web UI at `/` for quick testing.

## Features

- Manage clinic time slots via `POST /reset` and `GET /slots`
- Create bookings with validation and race-safe slot handling
- Prevent duplicate booking attempts using `Idempotency-Key`
- Rate-limit repeated booking attempts per email
- Retrieve, list, and cancel bookings
- Paginate booking history by email
- Serve a lightweight HTML UI directly from the Go server

## Tech stack

- Go
- PostgreSQL + `pgx`
- Standard-library HTTP server
- Embedded static HTML frontend

## Project structure

- `main.go` — server setup, routes, validation, logic, and middleware
- `index.html` — embedded frontend UI for loading slots and booking appointments
- `schema.sql` — database schema for slots and bookings
- `.gitignore` — repository ignore rules
- `go.mod` — Go module and dependencies

## Prerequisites

- Go 1.27+
- PostgreSQL database
- Valid `DATABASE_URL` and `API_KEY` environment variables

## Local setup

1. Clone the repository.
2. Create a PostgreSQL database.
3. Create the required tables.
4. Set the environment variables.
5. Run the app.

### Environment variables

Create a `.env` file in the project root:

```env
DATABASE_URL=postgres://username:password@localhost:5432/clinic
API_KEY=change-me
PORT=8080
```

### Database schema

Use the following schema in PostgreSQL:

```sql
CREATE TABLE slots (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    start_time TIMESTAMPTZ NOT NULL,
    end_time TIMESTAMPTZ NOT NULL
);

CREATE TABLE bookings (
    id TEXT PRIMARY KEY NOT NULL,
    slot_id TEXT NOT NULL REFERENCES slots(id),
    name TEXT NOT NULL,
    email TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'confirmed',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE idempotency_keys (
    key TEXT PRIMARY KEY,
    request_hash TEXT NOT NULL,
    status_code INT NOT NULL DEFAULT 200,
    response_body TEXT NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX bookings_confirmed_slot_unique
    ON bookings (slot_id)
    WHERE status = 'confirmed';
```

> The app’s idempotency logic relies on `idempotency_keys`, and the unique partial index on `bookings(slot_id)` is required for correct slot-booking conflict handling.

## Run the app

```bash
go mod download
go run .
```

The server listens on the port from `PORT` (default `8080`) and serves the UI at `http://localhost:8080/`.

## API authentication

All API routes except `/` and `/health` require the `X-API-Key` header.

Example:

```bash
curl -H "X-API-Key: change-me" http://localhost:8080/health
```

## API endpoints

### Health check

```http
GET /health
```

Response:

```json
{"status":"ok"}
```

### Reset slots

```http
POST /reset
```

Request body:

```json
{
  "slots": [
    {
      "id": "slot-1",
      "provider": "Dr. Smith",
      "start": "2026-11-01T09:00:00Z",
      "end": "2026-11-01T09:30:00Z"
    }
  ]
}
```

### List slots

```http
GET /slots?provider=Dr.%20Smith
```

Returns all slots for the given provider along with their availability status.

### Create booking

```http
POST /bookings
```

Headers:

```http
X-API-Key: <your-api-key>
Idempotency-Key: <unique-client-key>
Content-Type: application/json
```

Request body:

```json
{
  "slot_id": "slot-1",
  "name": "Jane Doe",
  "email": "jane@example.com"
}
```

Example:

```bash
curl -X POST http://localhost:8080/bookings \
  -H "X-API-Key: change-me" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" \
  -d '{"slot_id":"slot-1","name":"Jane Doe","email":"jane@example.com"}'
```

### List bookings by email

```http
GET /bookings?email=jane@example.com&limit=10&cursor=<optional-cursor>
```

### Get a booking

```http
GET /bookings/{id}
```

### Cancel a booking

```http
DELETE /bookings/{id}
```

## Example flow

1. Seed slots with `POST /reset`
2. Open the UI at `/`
3. Enter the API key
4. Select an available slot
5. Submit the booking form
6. Query bookings by email for history

## Notes

- Booking attempts are rate-limited to 5 attempts per email per minute.
- Slots that have already started cannot be booked.
- Duplicate idempotent requests with the same key and same payload are safely replayed.
- The HTML UI is embedded, so no separate frontend app is required.

## License

This repository does not currently declare a license. If you plan to publish or share it publicly, add an appropriate open-source license file.
