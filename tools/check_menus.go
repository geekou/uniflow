package main

import (
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", "E:\\cc\\uniflow\\uniflow.db")
	if err != nil {
		fmt.Println("open error:", err)
		return
	}
	defer db.Close()

	rows, _ := db.Query("PRAGMA table_info(menus)")
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk)
		fmt.Printf("col: %s (%s)\n", name, typ)
	}
	rows.Close()

	rows2, _ := db.Query("SELECT * FROM menus")
	cols, _ := rows2.Columns()
	fmt.Println("columns:", cols)
	for rows2.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals { ptrs[i] = &vals[i] }
		rows2.Scan(ptrs...)
		for i, c := range cols {
			fmt.Printf("  %s=%v ", c, vals[i])
		}
		fmt.Println()
	}
	rows2.Close()
}
