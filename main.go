package main

import (
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed templates/*
var templateFS embed.FS

const (
	uploadDir    = "./uploads"
	dbPath       = "./sqlite.db"
	maxFileSize  = 100 * 1024 * 1024
	fileLifetime = 7 * 24 * time.Hour
	idLength     = 6
)

type FileEntry struct {
	ID        string
	Filename  string
	Size      int64
	MimeType  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type UploadResponse struct {
	ID      string    `json:"id"`
	URL     string    `json:"url"`
	Expires time.Time `json:"expires"`
}

var (
	db   *sql.DB
	tmpl *template.Template
)

func main() {
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatal(err)
	}

	initDB()
	defer db.Close()

	initTemplates()

	go cleanupRoutine()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/upload", handleUpload)
	mux.HandleFunc("/f/", handleFileServe)

	srv := &http.Server{
		Addr:         ":6969",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("Server starting on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		log.Fatal(err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS files (
            id TEXT PRIMARY KEY,
            filename TEXT,
            size INTEGER,
            mime_type TEXT,
            created_at DATETIME,
            expires_at DATETIME
        )
    `)
	if err != nil {
		log.Fatal(err)
	}

	_, err = db.Exec(`
        CREATE INDEX IF NOT EXISTS idx_expires_at ON files(expires_at)
    `)
	if err != nil {
		log.Fatal(err)
	}
}

func initTemplates() {
	var err error
	tmpl, err = template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		log.Fatal(err)
	}
}

func generateID() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, idLength)
	rand.Read(b)
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "index.html", nil); err != nil {
		log.Printf("Template error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseMultipartForm(maxFileSize); err != nil {
		http.Error(w, "File too large", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Invalid file upload", http.StatusBadRequest)
		return
	}
	defer file.Close()

	id := generateID()
	for {
		var exists bool
		err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM files WHERE id = ?)", id).Scan(&exists)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if !exists {
			break
		}
		id = generateID()
	}

	mimeType := detectMimeType(header.Filename, file)
	if _, err := file.Seek(0, 0); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	expires := now.Add(fileLifetime)

	dst, err := os.Create(filepath.Join(uploadDir, id))
	if err != nil {
		http.Error(w, "Failed to create file", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	size, err := io.Copy(dst, file)
	if err != nil {
		os.Remove(filepath.Join(uploadDir, id))
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}

	_, err = db.Exec(
		"INSERT INTO files (id, filename, size, mime_type, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)",
		id, header.Filename, size, mimeType, now, expires,
	)
	if err != nil {
		os.Remove(filepath.Join(uploadDir, id))
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	response := UploadResponse{
		ID:      id,
		URL:     fmt.Sprintf("https://api.slop.sh/f/%s", id),
		Expires: expires,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleFileServe(w http.ResponseWriter, r *http.Request) {
	id := filepath.Base(r.URL.Path)

	var entry FileEntry
	err := db.QueryRow(
		"SELECT id, filename, size, mime_type, created_at, expires_at FROM files WHERE id = ?",
		id,
	).Scan(&entry.ID, &entry.Filename, &entry.Size, &entry.MimeType, &entry.CreatedAt, &entry.ExpiresAt)

	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	if time.Now().After(entry.ExpiresAt) {
		os.Remove(filepath.Join(uploadDir, entry.ID))
		db.Exec("DELETE FROM files WHERE id = ?", entry.ID)
		http.NotFound(w, r)
		return
	}

	file, err := os.Open(filepath.Join(uploadDir, entry.ID))
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	defer file.Close()

	disposition := "inline"
	if !isPreviewable(entry.MimeType) {
		disposition = "attachment"
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, entry.Filename))
	w.Header().Set("Content-Type", entry.MimeType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", entry.Size))
	w.Header().Set("Cache-Control", "public, max-age=604800")
	io.Copy(w, file)
}

func detectMimeType(filename string, file io.Reader) string {
	buffer := make([]byte, 512)
	n, err := file.Read(buffer)
	if err != nil && err != io.EOF {
		return "application/octet-stream"
	}

	mimeType := http.DetectContentType(buffer[:n])
	if mimeType == "application/octet-stream" {
		if ext := filepath.Ext(filename); ext != "" {
			if mtype := mime.TypeByExtension(ext); mtype != "" {
				return mtype
			}
		}
	}
	return mimeType
}

func isPreviewable(mimeType string) bool {
	previewable := []string{
		"image/", "video/", "audio/",
		"text/",
		"application/pdf",
		"application/json",
		"application/xml",
	}

	for _, prefix := range previewable {
		if strings.HasPrefix(mimeType, prefix) {
			return true
		}
	}
	return false
}

func cleanupRoutine() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		rows, err := db.Query("SELECT id FROM files WHERE expires_at < ?", time.Now())
		if err != nil {
			log.Printf("Cleanup query error: %v", err)
			continue
		}

		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				log.Printf("Cleanup scan error: %v", err)
				continue
			}

			if err := os.Remove(filepath.Join(uploadDir, id)); err != nil && !os.IsNotExist(err) {
				log.Printf("Failed to delete file %s: %v", id, err)
			}

			if _, err := db.Exec("DELETE FROM files WHERE id = ?", id); err != nil {
				log.Printf("Failed to delete database entry %s: %v", id, err)
			}
		}
		rows.Close()
	}
}
