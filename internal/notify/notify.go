// Package notify sends a desktop notification plus an optional phone push.
// Never fatal: a failed notify must not stop the rest of the run.
package notify

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const ntfyTopicEnv = "NTFY_TOPIC"

func ntfyServer() string {
	if s := os.Getenv("NTFY_SERVER"); s != "" {
		return s
	}
	return "https://ntfy.sh"
}

// loadEnv pulls .env in, so NTFY_TOPIC can live outside the shell environment.
// A missing or unreadable file is not an error: push is optional.
func loadEnv() {
	_ = godotenv.Load(".env")
}

// Desktop posts via notify-send, reporting whether it went out.
func Desktop(title, body, urgency string) bool {
	if _, err := exec.LookPath("notify-send"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "notify-send",
		"-u", urgency, "-a", "Job Tracker", title, body)
	return cmd.Run() == nil
}

// Phone pushes to ntfy when a topic is configured.
func Phone(message, title string) bool {
	loadEnv()
	topic := strings.TrimSpace(os.Getenv(ntfyTopicEnv))
	if topic == "" {
		return false
	}

	url := strings.TrimRight(ntfyServer(), "/") + "/" + topic
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(message))
	if err != nil {
		return false
	}
	req.Header.Set("Title", title)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// Send fans out to both channels, ignoring failures on either.
func Send(title, body, urgency string) {
	if urgency == "" {
		urgency = "normal"
	}
	Desktop(title, body, urgency)
	Phone(title+"\n"+body, "Job Tracker")
}
