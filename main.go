package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	tele "gopkg.in/telebot.v3"
)

const GEMINI_API_URL = "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash-lite:streamGenerateContent"

type GeminiRequest struct {
	SystemInstruction Content   `json:"system_instruction"`
	Contents          []Content `json:"contents"`
	SafetySettings    []Safety  `json:"safety_settings"`
}

type Safety struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

type Part struct {
	Text       string    `json:"text,omitempty"`
	InlineData *FileData `json:"inline_data,omitempty"`
}

type FileData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

type Content struct {
	Role  string `json:"role"`
	Parts []Part `json:"parts"`
}

type GeminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []Part `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

type SSEResponse struct {
	ID   string         `json:"id"`
	Text string         `json:"text"`
	Data GeminiResponse `json:"data"`
}

type Message struct {
	Role    string    `json:"role"`
	Message string    `json:"message"`
	Image   *FileData `json:"image,omitempty"`
}

type UserMessages struct {
	ID         int64     `json:"id"`
	TelegramID int64     `json:"telegramId"`
	Username   string    `json:"username"`
	Messages   []Message `json:"messages"`
}

func parseSSEResponse(line string) (*GeminiResponse, error) {
	if !strings.HasPrefix(line, "data: ") {
		return nil, nil
	}

	data := strings.TrimPrefix(line, "data: ")
	if data == "[DONE]" {
		return nil, nil
	}

	data = strings.TrimLeft(data, " \t\r\n\x00\x1b")

	var resp GeminiResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		log.Printf("Raw data causing error: %q\n", data)
		return nil, fmt.Errorf("unmarshal error: %v", err)
	}
	return &resp, nil
}

func loadEnvFile(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if len(line) == 0 || strings.HasPrefix(line, "#") {
			continue // Skip empty lines and comments
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		os.Setenv(key, value) // Set the environment variable
	}

	if err := scanner.Err(); err != nil {
		fmt.Println("Error reading file:", err)
	}
}

func getUserMessages(telegramID int64) ([]Message, error) {
	mokkyURL := os.Getenv("MOKKY_URL")
	if mokkyURL == "" {
		return nil, fmt.Errorf("MOKKY_URL environment variable is not set")
	}
	// Get messages for specific user directly
	resp, err := http.Get(fmt.Sprintf("%susers?telegramId=%d", mokkyURL, telegramID))
	if err != nil {
		return nil, fmt.Errorf("error getting messages from API: %v", err)
	}
	defer resp.Body.Close()

	var users []UserMessages
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return nil, fmt.Errorf("error decoding API response: %v", err)
	}

	if len(users) > 0 {
		return users[0].Messages, nil
	}

	return []Message{}, nil
}

func saveMessage(telegramID int64, userMsg, aiMsg string, sender *tele.User, imageData *FileData) error {
	mokkyURL := os.Getenv("MOKKY_URL")
	if mokkyURL == "" {
		return fmt.Errorf("MOKKY_URL environment variable is not set")
	}

	username := "no username " + fmt.Sprint(sender.ID)
	if sender.Username != "" {
		username = sender.Username
	} else if sender.FirstName != "" {
		username = sender.FirstName
	}

	resp, err := http.Get(fmt.Sprintf("%susers?telegramId=%d", mokkyURL, telegramID))
	if err != nil {
		return fmt.Errorf("error checking user existence: %v", err)
	}
	defer resp.Body.Close()

	var users []UserMessages
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return fmt.Errorf("error decoding API response: %v", err)
	}

	var messages []Message
	var method, url string

	if len(users) > 0 {
		messages = append(users[0].Messages, []Message{
			{
				Role:    "user",
				Message: userMsg,
				Image:   imageData, // Add image data if present
			},
			{
				Role:    "model",
				Message: aiMsg,
			},
		}...)
		method = "PATCH"
		url = fmt.Sprintf("https://4140c0059f1c791f.mokky.dev/users/%d", users[0].ID)
	} else {
		messages = []Message{
			{Role: "user", Message: userMsg},
			{Role: "model", Message: aiMsg},
		}
		method = "POST"
		url = "https://4140c0059f1c791f.mokky.dev/users"
	}

	userMsgs := UserMessages{
		TelegramID: telegramID,
		Username:   username,
		Messages:   messages,
	}

	jsonData, err := json.Marshal(userMsgs)
	if err != nil {
		return fmt.Errorf("error marshaling messages: %v", err)
	}

	req, err := http.NewRequest(method, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API returned non-200 status code: %d", resp.StatusCode)
	}

	return nil
}

func deleteUserHistory(telegramID int64) error {
	mokkyURL := os.Getenv("MOKKY_URL")
	if mokkyURL == "" {
		return fmt.Errorf("MOKKY_URL environment variable is not set")
	}

	// First get the user's record to get their ID
	resp, err := http.Get(fmt.Sprintf("%susers?telegramId=%d", mokkyURL, telegramID))
	if err != nil {
		return fmt.Errorf("error checking user existence: %v", err)
	}
	defer resp.Body.Close()

	var users []UserMessages
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return fmt.Errorf("error decoding API response: %v", err)
	}

	if len(users) == 0 {
		return fmt.Errorf("no history found for this user")
	}

	// Update the user's record with empty messages array
	url := fmt.Sprintf("%susers/%d", mokkyURL, users[0].ID)
	userMsgs := UserMessages{
		ID:         users[0].ID,
		TelegramID: telegramID,
		Username:   users[0].Username,
		Messages:   []Message{},
	}

	jsonData, err := json.Marshal(userMsgs)
	if err != nil {
		return fmt.Errorf("error marshaling messages: %v", err)
	}

	req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API returned non-200 status code: %d", resp.StatusCode)
	}

	return nil
}

func cleanupMessageHistory(telegramID int64, messages []Message) error {
	if len(messages) > 100 {
		log.Printf("Message history for user %d exceeds 100 messages, cleaning up...", telegramID)
		if err := deleteUserHistory(telegramID); err != nil {
			return fmt.Errorf("error cleaning up message history: %v", err)
		}
		log.Printf("Successfully cleaned up message history for user %d", telegramID)
	}
	return nil
}

func main() {
	loadEnvFile(".env")
	telegramToken := os.Getenv("TELEGRAM_TOKEN")
	geminiApiKey := os.Getenv("GEMINI_TOKEN")

	if telegramToken == "" || geminiApiKey == "" {
		log.Fatal("Please set TELEGRAM_TOKEN and GEMINI_API_KEY environment variables")
	}

	pref := tele.Settings{
		Token:  telegramToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		log.Fatal(err)
		return
	}

	b.Handle(tele.OnText, func(c tele.Context) error {
		userMsg := c.Text()

		c.Notify(tele.Typing)

		prevMessages, err := getUserMessages(c.Sender().ID)
		if err != nil {
			log.Printf("Error getting previous messages: %v\n", err)
		}

		if err := cleanupMessageHistory(c.Sender().ID, prevMessages); err != nil {
			log.Printf("Error during message cleanup: %v\n", err)
		}

		var contextMessages []Content
		for _, msg := range prevMessages {
			contextMessages = append(contextMessages, Content{
				Role:  msg.Role,
				Parts: []Part{{Text: msg.Message}},
			})
		}
		contextMessages = append(contextMessages, Content{
			Role:  "user",
			Parts: []Part{{Text: userMsg}},
		})

		reqBody := GeminiRequest{
			SystemInstruction: Content{
				Parts: []Part{
					{Text: "You are a helpful assistant. When responding, act as if you are continuing a conversation. Use only these punctuation marks: , . ? ! - \n" +
						"Do not use any other special characters or formatting. Keep your responses under 4096 characters. Respond with the actual content only, no need to add role prefixes."},
				},
			},
			Contents: contextMessages,
			SafetySettings: []Safety{
				{
					Category:  "HARM_CATEGORY_HARASSMENT",
					Threshold: "BLOCK_NONE",
				},
				{
					Category:  "HARM_CATEGORY_HATE_SPEECH",
					Threshold: "BLOCK_NONE",
				},
				{
					Category:  "HARM_CATEGORY_SEXUALLY_EXPLICIT",
					Threshold: "BLOCK_NONE",
				},
				{
					Category:  "HARM_CATEGORY_DANGEROUS_CONTENT",
					Threshold: "BLOCK_NONE",
				},
			},
		}

		jsonData, err := json.Marshal(reqBody)
		if err != nil {
			log.Println("Error marshaling request body:", err)
			return c.Send("Error processing your request")
		}

		url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent?key=%s", geminiApiKey)

		client := &http.Client{}
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Println("Error creating request:", err)
			return c.Send("Error creating request")
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Println("Error making request to Gemini API:", err)
			return c.Send("Error connecting to AI service")
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			log.Printf("Error Response Body: %s\n", body)
			return c.Send("Error: API returned non-200 status code")
		}

		var geminiResp GeminiResponse
		if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
			log.Println("Error decoding response:", err)
			return c.Send("Error decoding AI response")
		}

		if len(geminiResp.Candidates) > 0 && len(geminiResp.Candidates[0].Content.Parts) > 0 {
			responseText := geminiResp.Candidates[0].Content.Parts[0].Text
			telegramID := c.Sender().ID
			if err := saveMessage(telegramID, userMsg, responseText, c.Sender(), nil); err != nil {
				log.Printf("Error saving messages: %v\n", err)
			}
			return c.Send(responseText)
		}

		return c.Send("Sorry, I couldn't generate a response")
	})

	b.Handle(tele.OnPhoto, func(c tele.Context) error {
		photo := c.Message().Photo
		if photo == nil {
			return c.Send("No photo found in message")
		}

		c.Notify(tele.Typing)

		// Download the photo
		file, err := b.File(&photo.File)
		if err != nil {
			log.Printf("Error getting photo file: %v\n", err)
			return c.Send("Error processing image")
		}

		// Read the file data
		data := make([]byte, photo.File.FileSize)
		_, err = file.Read(data)
		if err != nil {
			log.Printf("Error reading photo data: %v\n", err)
			return c.Send("Error reading image")
		}

		// Convert to base64
		base64Data := base64.StdEncoding.EncodeToString(data)
		imageData := &FileData{
			MimeType: "image/jpeg",
			Data:     base64Data,
		}

		userMsg := c.Message().Caption
		if userMsg == "" {
			userMsg = "Image sent without caption"
		}

		reqBody := GeminiRequest{
			SystemInstruction: Content{
				Parts: []Part{
					{Text: "You are a helpful assistant. When analyzing images, provide detailed descriptions and answer any questions about them. Use only these punctuation marks: , . ? ! - \n"},
				},
			},
			Contents: []Content{
				{
					Role: "user",
					Parts: []Part{
						{Text: userMsg},
						{InlineData: imageData},
					},
				},
			},
			SafetySettings: []Safety{
				{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_NONE"},
				{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_NONE"},
				{Category: "HARM_CATEGORY_SEXUALLY_EXPLICIT", Threshold: "BLOCK_NONE"},
				{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_NONE"},
			},
		}

		jsonData, err := json.Marshal(reqBody)
		if err != nil {
			log.Println("Error marshaling request body:", err)
			return c.Send("Error processing your request")
		}

		url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent?key=%s", geminiApiKey)

		client := &http.Client{}
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Println("Error creating request:", err)
			return c.Send("Error creating request")
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Println("Error making request to Gemini API:", err)
			return c.Send("Error connecting to AI service")
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			log.Printf("Error Response Body: %s\n", body)
			return c.Send("Error: API returned non-200 status code")
		}

		var geminiResp GeminiResponse
		if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
			log.Println("Error decoding response:", err)
			return c.Send("Error decoding AI response")
		}

		if len(geminiResp.Candidates) > 0 && len(geminiResp.Candidates[0].Content.Parts) > 0 {
			responseText := geminiResp.Candidates[0].Content.Parts[0].Text
			telegramID := c.Sender().ID
			if err := saveMessage(telegramID, userMsg, responseText, c.Sender(), imageData); err != nil {
				log.Printf("Error saving messages: %v\n", err)
			}
			return c.Send(responseText)
		}

		return c.Send("Sorry, I couldn't generate a response")
	})

	b.Handle("/history", func(c tele.Context) error {
		c.Notify(tele.Typing)
		err := deleteUserHistory(c.Sender().ID)
		if err != nil {
			log.Printf("Error deleting user history: %v\n", err)
			return c.Send("Error deleting user history")
		}
		return c.Send("Your messsage history has been cleared!")
	})

	log.Println("Bot is running...")
	b.Start()
}
