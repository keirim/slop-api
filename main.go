package main

import (
    "crypto/rand"
    "database/sql"
    "fmt"
    "html/template"
    "io"
    "log"
    "net/http"
    "os"
    "path/filepath"
    "time"

    _ "github.com/mattn/go-sqlite3"
)

const (
    uploadDir     = "./uploads"
    dbPath        = "./sqlite.db"
    maxFileSize   = 100 * 1024 * 1024
    fileLifetime  = 7 * 24 * time.Hour
    idLength      = 6
)

type FileEntry struct {
    ID        string
    Filename  string
    Size      int64
    CreatedAt time.Time
    ExpiresAt time.Time
}

var db *sql.DB

func main() {
    if err := os.MkdirAll(uploadDir, 0755); err != nil {
        log.Fatal(err)
    }

    initDB()
    defer db.Close()

    go cleanupRoutine()

    http.HandleFunc("/", handleRoot)
    http.HandleFunc("/upload", handleUpload)
    http.HandleFunc("/f/", handleFileServe)

    log.Printf("Server starting on :6969")
    if err := http.ListenAndServe(":6969", nil); err != nil {
        log.Fatal(err)
    }
}

func initDB() {
    var err error
    db, err = sql.Open("sqlite3", dbPath)
    if err != nil {
        log.Fatal(err)
    }

    _, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS files (
            id TEXT PRIMARY KEY,
            filename TEXT,
            size INTEGER,
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

    tmpl := template.Must(template.New("index").Parse(`
<!DOCTYPE html>
<html>
<head>
    <title>slop.sh</title>
	<link rel="preconnect" href="https://fonts.googleapis.com">
	<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
	<link href="https://fonts.googleapis.com/css2?family=League+Spartan:wght@100..900&display=swap" rel="stylesheet">
    <style>
        body {
            background-color: #1a1a1a;
            color: #33ff33;
            font-family: 'League Spartan', monospace;
            line-height: 1.6;
            max-width: 800px;
            margin: 40px auto;
            padding: 20px;
        }
        pre {
            background-color: #2a2a2a;
            padding: 15px;
            border-radius: 5px;
            overflow-x: auto;
        }
        code {
            color: #ff9933;
        }
        h1, h2 {
            color: #ffffff;
            border-bottom: 1px solid #33ff33;
            padding-bottom: 10px;
        }
        .blink {
            animation: blink 1s infinite;
        }
        @keyframes blink {
            50% { opacity: 0; }
        }
        .prompt::before {
            content: "$ ";
            color: #ff9933;
        }
    </style>
</head>
<body>
    <h1>Slop.sh<span class="blink">_</span></h1>
    
    <h2>About</h2>
    <p>Slop.sh is a service that allows you to programmatically or manually upload files that will be available for 7 days. Files are automatically deleted after expiration.</p>
    
    <h2>Upload Limits</h2>
    <ul>
        <li>Maximum file size: 100MB</li>
        <li>File lifetime: 7 days</li>
        <li>No authentication required</li>
    </ul>

    <h2>API Usage</h2>
    
    <h3>Upload via cURL</h3>
    <pre class="prompt">curl -F "file=@local-file.txt" https://api.slop.sh/upload</pre>

    <h3>Download a file</h3>
    <pre class="prompt">curl -O https://api.slop.sh/f/YOUR_FILE_ID</pre>

    <h3>Upload via Python</h3>
    <pre>
import requests

files = {'file': open('local-file.txt', 'rb')}
response = requests.post('https://api.slop.sh/upload', files=files)
file_url = response.json()['url']</pre>

    <h3>Upload via Go</h3>
    <pre>
package main

import (
    "bytes"
    "io"
    "mime/multipart"
    "net/http"
    "os"
)

func main() {
    file, _ := os.Open("local-file.txt")
    defer file.Close()

    body := &bytes.Buffer{}
    writer := multipart.NewWriter(body)
    part, _ := writer.CreateFormFile("file", "local-file.txt")
    io.Copy(part, file)
    writer.Close()

    req, _ := http.NewRequest("POST", "https://api.slop.sh/upload", body)
    req.Header.Set("Content-Type", writer.FormDataHeader())
    
    client := &http.Client{}
    resp, _ := client.Do(req)
}</pre>

    <h2>Response Format</h2>
    <p>Successful uploads return JSON:</p>
    <pre>{
    "id": "Ax7b9q",
    "url": "https://api.slop.sh/f/Ax7b9q",
    "expires": "2024-01-11T15:04:05Z"
}</pre>
</body>
</html>
`))

    tmpl.Execute(w, nil)
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
        "INSERT INTO files (id, filename, size, created_at, expires_at) VALUES (?, ?, ?, ?, ?)",
        id, header.Filename, size, now, expires,
    )
    if err != nil {
        os.Remove(filepath.Join(uploadDir, id))
        http.Error(w, "Database error", http.StatusInternalServerError)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    fmt.Fprintf(w, `{
    "id": "%s",
    "url": "https://api.slop.sh/f/%s",
    "expires": "%s"
}`, id, id, expires.Format(time.RFC3339))
}

func handleFileServe(w http.ResponseWriter, r *http.Request) {
    id := filepath.Base(r.URL.Path)
    
    var entry FileEntry
    err := db.QueryRow(
        "SELECT id, filename, size, created_at, expires_at FROM files WHERE id = ?",
        id,
    ).Scan(&entry.ID, &entry.Filename, &entry.Size, &entry.CreatedAt, &entry.ExpiresAt)

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

    w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, entry.Filename))
    w.Header().Set("Content-Type", "application/octet-stream")
    w.Header().Set("Content-Length", fmt.Sprintf("%d", entry.Size))
    io.Copy(w, file)
}

func cleanupRoutine() {
    ticker := time.NewTicker(1 * time.Hour)
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
