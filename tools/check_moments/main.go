package main

import (
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", "E:\\cc\\uniflow\\uniflow.db")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer db.Close()

	rows, err := db.Query("SELECT id, content, media_urls FROM moments ORDER BY id")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var content, mediaURLs string
		rows.Scan(&id, &content, &mediaURLs)
		fmt.Printf("id=%d content=%q media_urls=%q\n", id, content, mediaURLs)
	}
}
