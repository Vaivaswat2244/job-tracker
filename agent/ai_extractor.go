package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sashabaranov/go-openai"
)

type JobDetails struct {
	CompanyName     string `json:"companyName"`
	RoleTitle       string `json:"roleTitle"`
	ApplicationDate string `json:"applicationDate"`
	Status          string `json:"status"`
	NextSteps       string `json:"nextSteps"`
	InterviewDate   string `json:"interviewDate"`
}

// getGroqClient creates an OpenAI client that points to Groq's servers
func getGroqClient() *openai.Client {
	apiKey := os.Getenv("GROQ_API_KEY")
	if apiKey == "" {
		panic("GROQ_API_KEY environment variable not set")
	}

	config := openai.DefaultConfig(apiKey)
	config.BaseURL = "https://api.groq.com/openai/v1"
	return openai.NewClientWithConfig(config)
}

func IsJobRelated(ctx context.Context, subject, bodySnippet string) (bool, error) {
	client := getGroqClient()

	prompt := fmt.Sprintf(`Analyze this email subject and snippet. 
Is this related to a job application, interview, or recruiting process? 
Reply exactly with "YES" or "NO".

Subject: %s
Snippet: %s`, subject, bodySnippet)

	resp, err := client.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: "llama-3.1-8b-instant",
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleUser,
					Content: prompt,
				},
			},
			Temperature: 0.0,
		},
	)

	if err != nil {
		return false, err
	}

	answer := resp.Choices[0].Message.Content
	return strings.Contains(strings.ToUpper(answer), "YES"), nil
}

func ExtractJobDetails(ctx context.Context, emailBody string) (*JobDetails, error) {
	client := getGroqClient()

	// FIXED: Removed all backticks from this string block so Go parses it correctly
	prompt := fmt.Sprintf(`Extract the job application details from the following email. 
You MUST respond with ONLY raw, valid JSON. Do not include markdown formatting or code blocks.

Use EXACTLY this schema:
{
  "companyName": "Name of the company",
  "roleTitle": "Title of the job role",
  "applicationDate": "Date of application if mentioned (YYYY-MM-DD), otherwise empty",
  "status": "Must be exactly one of: Applied, Screening, Interview Scheduled, Offer, Rejected, Ghosted",
  "nextSteps": "Any action items required by the candidate",
  "interviewDate": "Date/time of the interview if scheduled, otherwise empty"
}

Email:
%s`, emailBody)

	resp, err := client.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: "llama-3.3-70b-versatile",
			ResponseFormat: &openai.ChatCompletionResponseFormat{
				Type: openai.ChatCompletionResponseFormatTypeJSONObject,
			},
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleUser,
					Content: prompt,
				},
			},
			Temperature: 0.1,
		},
	)

	if err != nil {
		return nil, err
	}

	jsonString := resp.Choices[0].Message.Content
	var details JobDetails
	err = json.Unmarshal([]byte(jsonString), &details)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %v. Raw text: %s", err, jsonString)
	}

	return &details, nil
}
