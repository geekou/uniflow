package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	_ "modernc.org/sqlite"
)

var sanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9_\-.]`)

func sanitizeFilename(name string) string {
	cleaned := sanitizeRe.ReplaceAllString(name, "_")
	for strings.Contains(cleaned, "__") {
		cleaned = strings.ReplaceAll(cleaned, "__", "_")
	}
	cleaned = strings.Trim(cleaned, "_")
	if cleaned == "" {
		cleaned = "file"
	}
	return cleaned
}

func main() {
	db, err := sql.Open("sqlite", "E:\\cc\\uniflow\\uniflow.db")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer db.Close()

	uploadsDir := "E:\\cc\\uniflow\\uploads"

	// Get all moments with media_urls
	rows, err := db.Query("SELECT id, media_urls FROM moments WHERE media_urls != ''")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer rows.Close()

	type update struct {
		id      int
		newURLs string
	}
	var updates []update

	for rows.Next() {
		var id int
		var mediaURLs string
		rows.Scan(&id, &mediaURLs)
		fmt.Printf("Moment %d: %q\n", id, mediaURLs)

		// Split by comma (old separator)
		parts := strings.Split(mediaURLs, ",")
		fmt.Printf("  Split into %d parts\n", len(parts))

		// Check if any filename contains special chars that got split incorrectly
		// The pattern: /uploads/xxx_tmp_yyy followed by what looks like a URL param string
		// We need to rejoin parts that were incorrectly split
		var fixedParts []string
		i := 0
		for i < len(parts) {
			part := parts[i]
			// If this part starts with /uploads/ but next part doesn't, they may have been incorrectly split
			if strings.HasPrefix(part, "/uploads/") && i+1 < len(parts) && !strings.HasPrefix(parts[i+1], "/uploads/") {
				// This was split incorrectly - rejoin
				joined := part + "," + parts[i+1]
				fixedParts = append(fixedParts, joined)
				i += 2
				fmt.Printf("  Rejoined: %q\n", joined)
			} else if !strings.HasPrefix(part, "/uploads/") && len(fixedParts) == 0 {
				// Orphan part at beginning - skip
				fmt.Printf("  Skipping orphan: %q\n", part)
				i++
			} else if !strings.HasPrefix(part, "/uploads/") && len(fixedParts) > 0 {
				// Append to previous
				fixedParts[len(fixedParts)-1] = fixedParts[len(fixedParts)-1] + "," + part
				fmt.Printf("  Appended to previous: %q\n", fixedParts[len(fixedParts)-1])
				i++
			} else {
				fixedParts = append(fixedParts, part)
				i++
			}
		}

		fmt.Printf("  Fixed parts (%d):\n", len(fixedParts))

		// Now rename files and update URLs
		var newURLs []string
		for _, url := range fixedParts {
			if !strings.HasPrefix(url, "/uploads/") {
				fmt.Printf("  SKIP non-upload URL: %q\n", url)
				continue
			}
			oldFilename := strings.TrimPrefix(url, "/uploads/")
			oldPath := filepath.Join(uploadsDir, oldFilename)

			// Check if file exists
			if _, err := os.Stat(oldPath); os.IsNotExist(err) {
				fmt.Printf("  FILE NOT FOUND: %q\n", oldFilename)
				continue
			}

			// Sanitize the filename
			ext := strings.ToLower(filepath.Ext(oldFilename))
			baseName := strings.TrimSuffix(oldFilename, ext)
			newBaseName := sanitizeFilename(baseName)
			newFilename := newBaseName + ext

			if newFilename == oldFilename {
				// No change needed
				newURLs = append(newURLs, url)
				fmt.Printf("  OK: %q\n", oldFilename)
				continue
			}

			newPath := filepath.Join(uploadsDir, newFilename)
			// Check if new name already exists
			if _, err := os.Stat(newPath); err == nil {
				// File with clean name already exists, just update DB
				newURLs = append(newURLs, "/uploads/"+newFilename)
				fmt.Printf("  RENAME target exists, just updating DB: %q -> %q\n", oldFilename, newFilename)
				// Remove old file
				os.Remove(oldPath)
				continue
			}

			err := os.Rename(oldPath, newPath)
			if err != nil {
				fmt.Printf("  RENAME FAILED: %q -> %q: %v\n", oldFilename, newFilename, err)
				newURLs = append(newURLs, url) // Keep old URL
			} else {
				newURLs = append(newURLs, "/uploads/"+newFilename)
				fmt.Printf("  RENAMED: %q -> %q\n", oldFilename, newFilename)
			}
		}

		newURLsStr := strings.Join(newURLs, ",")
		if newURLsStr != mediaURLs {
			updates = append(updates, update{id: id, newURLs: newURLsStr})
		}
	}

	// Apply updates
	for _, u := range updates {
		_, err := db.Exec("UPDATE moments SET media_urls = ? WHERE id = ?", u.newURLs, u.id)
		if err != nil {
			fmt.Printf("UPDATE FAILED for moment %d: %v\n", u.id, err)
		} else {
			fmt.Printf("UPDATED moment %d: %q\n", u.id, u.newURLs)
		}
	}

	// Also clean up any tmp_ files that are orphans (not referenced in DB)
	fmt.Println("\n--- Checking for orphan files ---")
	dbFiles := map[string]bool{}
	rows2, _ := db.Query("SELECT media_urls FROM moments WHERE media_urls != ''")
	defer rows2.Close()
	for rows2.Next() {
		var mu string
		rows2.Scan(&mu)
		for _, p := range strings.Split(mu, ",") {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, "/uploads/") {
				dbFiles[strings.TrimPrefix(p, "/uploads/")] = true
			}
		}
	}
	// Also check posts
	rows3, _ := db.Query("SELECT thumb_url FROM posts WHERE thumb_url != ''")
	defer rows3.Close()
	for rows3.Next() {
		var tu string
		rows3.Scan(&tu)
		if strings.HasPrefix(tu, "/uploads/") {
			dbFiles[strings.TrimPrefix(tu, "/uploads/")] = true
		}
	}
	// Check banner
	rows4, _ := db.Query("SELECT value FROM system_settings WHERE key='banner_url' AND value != ''")
	defer rows4.Close()
	for rows4.Next() {
		var bu string
		rows4.Scan(&bu)
		if strings.HasPrefix(bu, "/uploads/") {
			dbFiles[strings.TrimPrefix(bu, "/uploads/")] = true
		}
	}

	entries, _ := os.ReadDir(uploadsDir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !dbFiles[e.Name()] {
			fmt.Printf("  ORPHAN: %s\n", e.Name())
		}
	}
}
