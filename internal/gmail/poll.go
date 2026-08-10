package gmail

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"google.golang.org/api/gmail/v1"
)

// MaxMessages caps one run. The window is a few days of mail, not a mailbox
// archive; a cap keeps a misconfigured query from pulling thousands of
// messages and burning quota.
const MaxMessages = 200

// Poll reads recent mail and applies what it can. Returns a summary.
func Poll(ctx context.Context, conn *sql.DB, svc *gmail.Service, cfg Config, since time.Duration) (Result, error) {
	var result Result

	query := cfg.Query
	if since > 0 {
		// Gmail's newer_than takes whole days; round up so a 36h window does
		// not silently become one day and miss the older half.
		days := int((since + 24*time.Hour - 1) / (24 * time.Hour))
		query = fmt.Sprintf("%s newer_than:%dd", query, days)
	}

	var ids []*gmail.Message
	call := svc.Users.Messages.List("me").Q(query).MaxResults(int64(MaxMessages))
	for {
		page, err := call.Context(ctx).Do()
		if err != nil {
			return result, fmt.Errorf("list messages: %w", err)
		}
		ids = append(ids, page.Messages...)
		if page.NextPageToken == "" || len(ids) >= MaxMessages {
			break
		}
		call = svc.Users.Messages.List("me").Q(query).
			MaxResults(int64(MaxMessages)).PageToken(page.NextPageToken)
	}
	if len(ids) > MaxMessages {
		ids = ids[:MaxMessages]
	}

	now := time.Now().UTC()
	for _, stub := range ids {
		// Skip the fetch entirely for anything already recorded: the id is
		// enough to know, and message.get is the expensive call.
		var seen string
		if err := conn.QueryRow(
			"SELECT gmail_id FROM mail_messages WHERE gmail_id = ?", stub.Id).Scan(&seen); err == nil {
			result.Skipped++
			continue
		}

		full, err := svc.Users.Messages.Get("me", stub.Id).Format("full").Context(ctx).Do()
		if err != nil {
			return result, fmt.Errorf("get message %s: %w", stub.Id, err)
		}
		result.Scanned++

		action, err := Ingest(conn, fromAPI(full), now)
		if err != nil {
			if AlreadySeen(err) {
				result.Skipped++
				continue
			}
			return result, err
		}
		switch action {
		case ActionApplied:
			result.Applied++
		case ActionRejected:
			result.Rejected++
		case ActionReplied:
			result.Replied++
		case ActionQueued:
			result.Queued++
		}
	}
	return result, nil
}

// fromAPI flattens a Gmail message into what the classifier needs.
func fromAPI(m *gmail.Message) Message {
	out := Message{ID: m.Id, ThreadID: m.ThreadId, Snippet: m.Snippet}
	if m.InternalDate > 0 {
		out.ReceivedAt = time.UnixMilli(m.InternalDate).UTC().Format("2006-01-02T15:04:05-07:00")
	}
	if m.Payload == nil {
		return out
	}
	for _, h := range m.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			out.From = h.Value
		case "subject":
			out.Subject = h.Value
		}
	}
	out.Body = plainText(m.Payload, 0)
	return out
}

// plainText walks the MIME tree for the first text/plain part, falling back to
// HTML with the tags stripped. Depth is bounded because a malformed message can
// nest parts arbitrarily.
func plainText(part *gmail.MessagePart, depth int) string {
	if part == nil || depth > 8 {
		return ""
	}
	if strings.HasPrefix(part.MimeType, "text/plain") && part.Body != nil && part.Body.Data != "" {
		return decode(part.Body.Data)
	}
	for _, sub := range part.Parts {
		if text := plainText(sub, depth+1); text != "" {
			return text
		}
	}
	if strings.HasPrefix(part.MimeType, "text/html") && part.Body != nil && part.Body.Data != "" {
		return stripTags(decode(part.Body.Data))
	}
	return ""
}

func decode(data string) string {
	raw, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(data)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(data)
		if err != nil {
			return ""
		}
	}
	return string(raw)
}

// stripTags is enough to get phrases out of an HTML mail for matching. It is
// not sanitisation: nothing here is ever rendered.
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>':
			if depth > 0 {
				depth--
			}
			b.WriteByte(' ')
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}
