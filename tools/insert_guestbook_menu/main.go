package main

import (
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
)

func main() {
	dbPath := "uniflow.db"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer db.Close()

	// Fix corrupted menu name for id=1
	db.Exec("UPDATE menus SET name='首页' WHERE id=1")

	// Delete test menu id=8
	db.Exec("DELETE FROM menus WHERE id=8")

	// Check if guestbook menu already exists
	var count int
	db.QueryRow("SELECT COUNT(*) FROM menus WHERE url='/guestbook'").Scan(&count)
	if count > 0 {
		fmt.Println("guestbook menu already exists")
		return
	}

	var maxOrder int
	db.QueryRow("SELECT COALESCE(MAX(order_num), 0) FROM menus").Scan(&maxOrder)

	_, err = db.Exec("INSERT INTO menus (name, url, icon, order_num) VALUES (?, ?, ?, ?)",
		"留言板", "/guestbook", "fa-regular fa-envelope", maxOrder+1)
	if err != nil {
		fmt.Println("insert error:", err)
		return
	}
	fmt.Println("guestbook menu inserted, order_num:", maxOrder+1)
}
