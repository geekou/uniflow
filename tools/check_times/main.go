package main

import (
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
)

func main() {
	home, _ := os.UserHomeDir()
	dbPath := filepath.Join("E:\\cc\\uniflow", "uniflow.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Println("open error:", err)
		return
	}
	defer db.Close()

	// Check guestbook times
	rows, _ := db.Query("SELECT id, author, created_at FROM guestbook ORDER BY id DESC LIMIT 3")
	fmt.Println("=== Guestbook ===")
	for rows.Next() {
		var id int
		var author, created string
		rows.Scan(&id, &author, &created)
		fmt.Printf("id=%d author=%s created_at=%s\n", id, author, created)
	}

	// Check posts times
	rows2, _ := db.Query("SELECT id, title, created_at, updated_at FROM posts ORDER BY id DESC LIMIT 3")
	fmt.Println("\n=== Posts ===")
	for rows2.Next() {
		var id int
		var title, created, updated string
		rows2.Scan(&id, &title, &created, &updated)
		fmt.Printf("id=%d title=%s created_at=%s updated_at=%s\n", id, title, created, updated)
	}

	// Check current time from DB
	var now string
	db.QueryRow("SELECT datetime('now')").Scan(&now)
	fmt.Printf("\nSQLite UTC now: %s\n", now)
	db.QueryRow("SELECT datetime('now', 'localtime')").Scan(&now)
	fmt.Printf("SQLite local now: %s\n", now)
	_ = home
}
