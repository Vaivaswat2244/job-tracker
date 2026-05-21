package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
)

// ParsedEmail holds our extracted data for the AI to process
type ParsedEmail struct {
	GmailID  string
	ThreadID string
	Subject  string
	Sender   string
	Date     string
	Body     string
}

// Retrieve a token, saves the token, then returns the generated client.
func GetClient(config *oauth2.Config) *http.Client {
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	return config.Client(context.Background(), tok)
}

func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		log.Fatalf("Unable to read authorization code: %v", err)
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web: %v", err)
	}
	return tok
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func saveToken(path string, token *oauth2.Token) {
	fmt.Printf("Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

// Recursively search for text/plain parts in the MIME tree
func ExtractPlainText(parts []*gmail.MessagePart) string {
	var textBody string
	for _, part := range parts {
		if part.MimeType == "text/plain" {
			data, err := base64.URLEncoding.DecodeString(part.Body.Data)
			if err == nil {
				textBody += string(data)
			}
		} else if len(part.Parts) > 0 {
			textBody += ExtractPlainText(part.Parts)
		}
	}
	return textBody
}

func PollNewEmails(srv *gmail.Service, afterTimestamp int64) ([]ParsedEmail, error) {
	query := ""
	if afterTimestamp > 0 {
		query = fmt.Sprintf("after:%d", afterTimestamp)
	}

	r, err := srv.Users.Messages.List("me").Q(query).Do()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve messages: %v", err)
	}

	var parsedEmails []ParsedEmail

	for _, msgItem := range r.Messages {
		msg, err := srv.Users.Messages.Get("me", msgItem.Id).Format("full").Do()
		if err != nil {
			log.Printf("Error fetching message %s: %v", msgItem.Id, err)
			continue
		}

		email := ParsedEmail{
			GmailID:  msg.Id,
			ThreadID: msg.ThreadId,
		}

		// Extract Headers
		for _, header := range msg.Payload.Headers {
			switch strings.ToLower(header.Name) {
			case "subject":
				email.Subject = header.Value
			case "from":
				email.Sender = header.Value
			case "date":
				email.Date = header.Value
			}
		}

		// Extract Body
		if len(msg.Payload.Parts) > 0 {
			email.Body = ExtractPlainText(msg.Payload.Parts)
		} else if msg.Payload.Body != nil && msg.Payload.Body.Data != "" {
			// Handle simple, single-part emails
			data, decErr := base64.URLEncoding.DecodeString(msg.Payload.Body.Data)
			if decErr == nil {
				email.Body = string(data)
			}
		}

		parsedEmails = append(parsedEmails, email)
	}

	return parsedEmails, nil
}
