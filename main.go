package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

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

	// 5. Print success message
	fmt.Println("connected")
}
