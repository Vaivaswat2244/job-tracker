package db

import (
	"database/sql"
	"log"

	_ "github.com/mattn/go-sqlite3"
)

// InitDB creates the jobs and emails tables if they don't exist
func InitDB(filepath string) *sql.DB {
	db, err := sql.Open("sqlite3", filepath)
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}

	createTablesSQL := `
	CREATE TABLE IF NOT EXISTS jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		company TEXT,
		role TEXT,
		status TEXT,
		applied_date TEXT,
		last_updated DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS emails (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER,
		gmail_thread_id TEXT UNIQUE,
		subject TEXT,
		received_at TEXT,
		FOREIGN KEY(job_id) REFERENCES jobs(id)
	);`

	_, err = db.Exec(createTablesSQL)
	if err != nil {
		log.Fatalf("Error creating tables: %v", err)
	}

	return db
}

// UpsertJob handles inserting new jobs or updating existing ones based on ThreadID
func UpsertJob(db *sql.DB, threadID string, company string, role string, status string, receivedAt string, subject string) {
	var jobID int
	err := db.QueryRow("SELECT job_id FROM emails WHERE gmail_thread_id = ?", threadID).Scan(&jobID)

	if err == sql.ErrNoRows {
		res, err := db.Exec("INSERT INTO jobs (company, role, status, applied_date) VALUES (?, ?, ?, ?)", company, role, status, receivedAt)
		if err != nil {
			log.Printf("Error inserting job: %v", err)
			return
		}
		newJobID, _ := res.LastInsertId()
		db.Exec("INSERT INTO emails (job_id, gmail_thread_id, subject, received_at) VALUES (?, ?, ?, ?)", newJobID, threadID, subject, receivedAt)
		log.Printf("Added new job: %s at %s", role, company)

	} else if err == nil {
		db.Exec("UPDATE jobs SET status = ?, last_updated = CURRENT_TIMESTAMP WHERE id = ?", status, jobID)
		log.Printf("Updated job %d to status: %s", jobID, status)
	}
}
