package main

import (
	"bufio"
	"bytes"
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

const (
	GEMINI_API_URL    = "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash-lite:streamGenerateContent"
	GEMINI_UPLOAD_URL = "https://generativelanguage.googleapis.com/upload/v1beta/files"
)

type GeminiRequest struct {
	SystemInstruction Content   `json:"system_instruction"`
	Contents          []Content `json:"contents"`
	SafetySettings    []Safety  `json:"safety_settings"`
}

type Safety struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

type FileData struct {
	MimeType string `json:"mime_type"`
	FileURI  string `json:"file_uri"`
}

type Part struct {
	Text     string    `json:"text,omitempty"`
	FileData *FileData `json:"file_data,omitempty"`
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
	Role    string `json:"role"`
	Message string `json:"message"`
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

func uploadAudioToGemini(audioData []byte, mimeType string, apiKey string) (string, error) {
	// Initial resumable upload request
	startReq, err := http.NewRequest("POST", fmt.Sprintf("%s?key=%s", GEMINI_UPLOAD_URL, apiKey), strings.NewReader(`{"file":{"display_name":"AUDIO"}}`))
	if err != nil {
		return "", fmt.Errorf("error creating upload request: %v", err)
	}

	startReq.Header.Set("X-Goog-Upload-Protocol", "resumable")
	startReq.Header.Set("X-Goog-Upload-Command", "start")
	startReq.Header.Set("X-Goog-Upload-Header-Content-Length", fmt.Sprintf("%d", len(audioData)))
	startReq.Header.Set("X-Goog-Upload-Header-Content-Type", mimeType)
	startReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(startReq)
	if err != nil {
		return "", fmt.Errorf("error starting upload: %v", err)
	}
	defer resp.Body.Close()

	uploadURL := resp.Header.Get("X-Goog-Upload-URL")
	if uploadURL == "" {
		return "", fmt.Errorf("no upload URL received")
	}

	// Upload the actual audio data
	uploadReq, err := http.NewRequest("POST", uploadURL, bytes.NewReader(audioData))
	if err != nil {
		return "", fmt.Errorf("error creating data upload request: %v", err)
	}

	uploadReq.Header.Set("Content-Length", fmt.Sprintf("%d", len(audioData)))
	uploadReq.Header.Set("X-Goog-Upload-Offset", "0")
	uploadReq.Header.Set("X-Goog-Upload-Command", "upload, finalize")

	resp, err = client.Do(uploadReq)
	if err != nil {
		return "", fmt.Errorf("error uploading data: %v", err)
	}
	defer resp.Body.Close()

	var fileInfo struct {
		File struct {
			URI string `json:"uri"`
		} `json:"file"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&fileInfo); err != nil {
		return "", fmt.Errorf("error decoding file info: %v", err)
	}

	return fileInfo.File.URI, nil
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

func saveMessage(telegramID int64, userMsg, aiMsg string, sender *tele.User) error {
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
			{Role: "user", Message: userMsg},
			{Role: "model", Message: aiMsg},
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
		log.Printf("Sending request: %s\n", jsonData)
		url := fmt.Sprintf("%s?key=%s&alt=sse", GEMINI_API_URL, geminiApiKey)

		client := &http.Client{}
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Println("Error creating request:", err)
			return c.Send("Error creating request")
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		resp, err := client.Do(req)
		if err != nil {
			log.Println("Error making request to Gemini API:", err)
			return c.Send("Error connecting to AI service")
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		var fullResponse strings.Builder
		var msg *tele.Message

		var lastUpdate time.Time
		updateInterval := 500 * time.Millisecond
		var firstChunk bool = true

		for scanner.Scan() {
			line := scanner.Text()

			log.Printf("Raw line: %s\n", line)

			if line == "" {
				continue
			}

			geminiResp, err := parseSSEResponse(line)
			if err != nil {
				log.Printf("Error parsing SSE response: %v\n", err)
				continue
			}

			if geminiResp != nil && len(geminiResp.Candidates) > 0 {
				candidate := geminiResp.Candidates[0]
				if len(candidate.Content.Parts) > 0 {
					chunk := candidate.Content.Parts[0].Text
					log.Printf("Received chunk: %s\n", chunk)
					fullResponse.WriteString(chunk)

					if firstChunk {
						msg, err = b.Send(c.Recipient(), chunk, &tele.SendOptions{
							ParseMode: tele.ModeMarkdown,
						})
						if err != nil {
							log.Printf("Error sending first chunk: %v\n", err)
							return err
						}
						firstChunk = false
						lastUpdate = time.Now()
						continue
					}

					if time.Since(lastUpdate) > updateInterval {
						_, err := b.Edit(msg, fullResponse.String(), &tele.SendOptions{
							ParseMode: tele.ModeMarkdown,
						})
						if err != nil {
							log.Printf("Error updating message: %v\n", err)
						}
						lastUpdate = time.Now()
					}
				}
			}
		}

		if fullResponse.Len() > 0 {
			finalText := fullResponse.String() + "\u200B"
			_, err := b.Edit(msg, finalText, &tele.SendOptions{
				ParseMode: tele.ModeMarkdown,
			})
			if err != nil {
				log.Printf("Error sending final update: %v\n", err)
			}

			telegramID := c.Sender().ID
			if err := saveMessage(telegramID, userMsg, fullResponse.String(), c.Sender()); err != nil {
				log.Printf("Error saving messages: %v\n", err)
			}

			return nil
		}

		if resp.StatusCode != http.StatusOK {
			log.Printf("Unexpected status code: %d\n", resp.StatusCode)
			if msg != nil {
				_, err := b.Edit(msg, "Error: API returned non-200 status code", &tele.SendOptions{
					ParseMode: tele.ModeMarkdown,
				})
				if err != nil {
					log.Printf("Error sending error message: %v\n", err)
				}
			} else {
				err := c.Send("Error: API returned non-200 status code")
				if err != nil {
					log.Printf("Error sending error message: %v\n", err)
				}
			}
			return nil
		}
		if msg == nil {
			return c.Send("Sorry, I couldn't generate a response")
		}

		return nil
	})

	b.Handle(tele.OnVoice, func(c tele.Context) error {
		voice := c.Message().Voice

		file, err := b.File(&voice.File)
		if err != nil {
			log.Printf("Error getting voice file: %v\n", err)
			return c.Send("Error processing voice message")
		}

		data, err := io.ReadAll(file)
		if err != nil {
			log.Printf("Error downloading voice file: %v\n", err)
			return c.Send("Error downloading voice message")
		}

		fileURI, err := uploadAudioToGemini(data, "audio/ogg", geminiApiKey)
		if err != nil {
			log.Printf("Error uploading audio to Gemini: %v\n", err)
			return c.Send("Error processing voice message")
		}

		var client = &http.Client{} // Add this at the start of the handler
		var url string              // Add this at the start of the handler

		// First request to transcribe
		transcriptionReq := GeminiRequest{
			SystemInstruction: Content{
				Parts: []Part{
					{Text: "You are a transcription assistant. Only output the transcribed text without any additional commentary."},
				},
			},
			Contents: []Content{
				{
					Role: "user",
					Parts: []Part{
						{
							FileData: &FileData{
								MimeType: "audio/ogg",
								FileURI:  fileURI,
							},
						},
					},
				},
			},
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

		// Get transcription first
		transcription := ""
		jsonData, err := json.Marshal(transcriptionReq)
		if err != nil {
			log.Println("Error marshaling transcription request:", err)
			return c.Send("Error processing your request")
		}

		// Make transcription request
		url = fmt.Sprintf("%s?key=%s", GEMINI_API_URL, geminiApiKey)
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Println("Error creating transcription request:", err)
			return c.Send("Error creating request")
		}

		req.Header.Set("Content-Type", "application/json")
		resp, err = client.Do(req)
		if err != nil {
			log.Println("Error making transcription request:", err)
			return c.Send("Error transcribing audio")
		}
		defer resp.Body.Close()

		var transcriptionResp GeminiResponse
		if err := json.NewDecoder(resp.Body).Decode(&transcriptionResp); err != nil {
			log.Printf("Error decoding transcription response: %v\n", err)
			return c.Send("Error processing transcription")
		}

		if len(transcriptionResp.Candidates) > 0 && len(transcriptionResp.Candidates[0].Content.Parts) > 0 {
			transcription = transcriptionResp.Candidates[0].Content.Parts[0].Text
		}

		reqBody := GeminiRequest{
			SystemInstruction: Content{
				Parts: []Part{
					{Text: "You are a helpful assistant. When responding, act as if you are continuing a conversation. Use only these punctuation marks: , . ? ! - \n" +
						"Do not use any other special characters or formatting. Keep your responses under 4096 characters. Respond with the actual content only, no need to add role prefixes."},
				},
			},
			Contents: []Content{
				{
					Role: "user",
					Parts: []Part{
						{Text: transcription}, // Use the transcription as the input text
					},
				},
			},
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

		url := fmt.Sprintf("%s?key=%s&alt=sse", GEMINI_API_URL, geminiApiKey)
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Println("Error creating request:", err)
			return c.Send("Error creating request")
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			log.Println("Error making request to Gemini API:", err)
			return c.Send("Error connecting to AI service")
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		var fullResponse strings.Builder
		var msg *tele.Message

		var lastUpdate time.Time
		updateInterval := 500 * time.Millisecond
		var firstChunk bool = true

		for scanner.Scan() {
			line := scanner.Text()
			log.Printf("Raw line: %s\n", line)

			if line == "" {
				continue
			}

			geminiResp, err := parseSSEResponse(line)
			if err != nil {
				log.Printf("Error parsing SSE response: %v\n", err)
				continue
			}

			if geminiResp != nil && len(geminiResp.Candidates) > 0 {
				candidate := geminiResp.Candidates[0]
				if len(candidate.Content.Parts) > 0 {
					chunk := candidate.Content.Parts[0].Text
					log.Printf("Received chunk: %s\n", chunk)
					fullResponse.WriteString(chunk)

					if firstChunk {
						msg, err = b.Send(c.Recipient(), chunk, &tele.SendOptions{
							ParseMode: tele.ModeMarkdown,
						})
						if err != nil {
							log.Printf("Error sending first chunk: %v\n", err)
							return err
						}
						firstChunk = false
						lastUpdate = time.Now()
						continue
					}

					if time.Since(lastUpdate) > updateInterval {
						_, err := b.Edit(msg, fullResponse.String(), &tele.SendOptions{
							ParseMode: tele.ModeMarkdown,
						})
						if err != nil {
							log.Printf("Error updating message: %v\n", err)
						}
						lastUpdate = time.Now()
					}
				}
			}
		}

		if fullResponse.Len() > 0 {
			finalText := fullResponse.String() + "\u200B"
			_, err := b.Edit(msg, finalText, &tele.SendOptions{
				ParseMode: tele.ModeMarkdown,
			})
			if err != nil {
				log.Printf("Error sending final update: %v\n", err)
			}

			// Save the transcription and response to user history
			if err := saveMessage(c.Sender().ID, transcription, fullResponse.String(), c.Sender()); err != nil {
				log.Printf("Error saving messages: %v\n", err)
			}

			return nil
		}

		if resp.StatusCode != http.StatusOK {
			log.Printf("Unexpected status code: %d\n", resp.StatusCode)
			return c.Send("Error: API returned non-200 status code")
		}

		return c.Send("Sorry, I couldn't process your voice message")
	})

	b.Handle("/history", func(c tele.Context) error {
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
