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

	rows, err := db.Query("PRAGMA table_info(menus)")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk)
		fmt.Printf("col %d: %s %s notnull=%d dflt=%v pk=%d\n", cid, name, typ, notNull, dflt, pk)
	}

	// Check existing menus
	rows2, _ := db.Query("SELECT id, name, url, icon FROM menus ORDER BY id")
	defer rows2.Close()
	fmt.Println("\nExisting menus:")
	for rows2.Next() {
		var id int
		var name, url, icon string
		rows2.Scan(&id, &name, &url, &icon)
		fmt.Printf("  id=%d name=%s url=%s icon=%s\n", id, name, url, icon)
	}
}
