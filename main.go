package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"job-tracker/agent"
	"job-tracker/db"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

func main() {
	ctx := context.Background()

	// 1. Initialize SQLite Database
	database := db.InitDB("tracker.db")
	defer database.Close()
	fmt.Println("✅ Database initialized (tracker.db)")

	// 2. Initialize Gmail API
	b, err := os.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("Unable to read client secret file: %v", err)
	}
	config, err := google.ConfigFromJSON(b, gmail.GmailReadonlyScope)
	if err != nil {
		log.Fatalf("Unable to parse client secret file to config: %v", err)
	}
	gmailClient := agent.GetClient(config)
	gmailSrv, err := gmail.NewService(ctx, option.WithHTTPClient(gmailClient))
	if err != nil {
		log.Fatalf("Unable to retrieve Gmail client: %v", err)
	}
	fmt.Println("✅ Gmail API authenticated")

	// --- THE PIPELINE ---

	// 3. Poll Emails (last 24 hours)
	fmt.Println("\n🔍 Polling Gmail for recent messages...")
	oneDayAgo := time.Now().Add(-24 * time.Hour).Unix()

	emails, err := agent.PollNewEmails(gmailSrv, oneDayAgo)
	if err != nil {
		log.Fatalf("Failed to poll emails: %v", err)
	}
	fmt.Printf("📬 Found %d recent emails. Starting analysis...\n\n", len(emails))

	// 4. Process Each Email
	for _, email := range emails {
		snippet := email.Body
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}

		// Call Groq classification
		isJob, err := agent.IsJobRelated(ctx, email.Subject, snippet)
		if err != nil {
			log.Printf("⚠️ Classification error on '%s': %v", email.Subject, err)
			time.Sleep(5 * time.Second) // Backoff on error
			continue
		}

		if isJob {
			fmt.Printf("🎯 [JOB MATCH] Processing: %s\n", email.Subject)

			// Call Groq JSON extraction
			details, err := agent.ExtractJobDetails(ctx, email.Body)
			if err != nil {
				log.Printf("⚠️ Extraction error on '%s': %v", email.Subject, err)
				continue
			}

			// Save to DB
			db.UpsertJob(
				database,
				email.ThreadID,
				details.CompanyName,
				details.RoleTitle,
				details.Status,
				email.Date,
				email.Subject,
			)
		} else {
			// Uncomment if you want to see skips
			// fmt.Printf("⏭️  [SKIPPED] %s\n", email.Subject)
		}

		// Groq free tier limit is 30 Requests Per Minute. 2 seconds keeps us perfectly safe.
		time.Sleep(2 * time.Second)
	}

	fmt.Println("\n✨ Pipeline run complete.")
}
