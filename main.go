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

const GEMINI_API_URL = "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:streamGenerateContent"

type GeminiRequest struct {
	SystemInstruction Content   `json:"system_instruction"`
	Contents          []Content `json:"contents"`
	SafetySettings    []Safety  `json:"safety_settings"`
	Tools             []Tool    `json:"tools,omitempty"`
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

type GenerationConfig struct {
	ResponseModalities []string `json:"responseModalities"`
}

type ImageGenerationRequest struct {
	Contents         []Content        `json:"contents"`
	GenerationConfig GenerationConfig `json:"generationConfig"`
}

type WebSource struct {
	Uri   string `json:"uri"`
	Title string `json:"title"`
}

type GroundingChunk struct {
	Web WebSource `json:"web"`
}

type GroundingMetadata struct {
	GroundingChunks []GroundingChunk `json:"groundingChunks"`
}

type Tool struct {
	GoogleSearch struct{} `json:"google_search"`
}

type GeminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []Part `json:"parts"`
		} `json:"content"`
		GroundingMetadata GroundingMetadata `json:"groundingMetadata"`
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

type DynamicRetrievalConfig struct {
	Mode             string  `json:"mode"`
	DynamicThreshold float64 `json:"dynamic_threshold"`
}

type GoogleSearch struct {
	DynamicRetrievalConfig DynamicRetrievalConfig `json:"dynamic_retrieval_config"`
}

func splitMessageIntoChunks(message string, maxSize int) []string {
	if len(message) <= maxSize {
		return []string{message}
	}

	var chunks []string
	for len(message) > 0 {
		if len(message) <= maxSize {
			chunks = append(chunks, message)
			break
		}

		lastNewline := strings.LastIndex(message[:maxSize], "\n")
		splitIndex := lastNewline
		if splitIndex == -1 {
			splitIndex = strings.LastIndex(message[:maxSize], " ")
		}

		if splitIndex == -1 {
			splitIndex = maxSize
		} else {
			splitIndex++
		}

		chunks = append(chunks, message[:splitIndex])
		message = message[splitIndex:]
	}

	return chunks
}

func sendChunkedMessage(c tele.Context, message string) error {
	if message == "" {
		return fmt.Errorf("empty message provided")
	}

	chunks := splitMessageIntoChunks(message, 4096)

	for i, chunk := range chunks {
		if chunk == "" {
			continue
		}

		err := c.Send(chunk)
		if err != nil {
			log.Printf("Error sending chunk %d: %v", i+1, err)
			time.Sleep(1 * time.Second)
			err = c.Send(chunk)
			if err != nil {
				return fmt.Errorf("failed to send message chunk %d after retry: %v", i+1, err)
			}
		}

		if i < len(chunks)-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}

	return nil
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
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		os.Setenv(key, value)
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

func saveMessage(telegramID int64, userMsg, aiMsg string, sender *tele.User, imageData *FileData, imageInUserMsg bool) error {
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

	var userImage, modelImage *FileData
	if imageInUserMsg {
		userImage = imageData
		modelImage = nil
	} else {
		userImage = nil
		modelImage = imageData
	}

	if len(users) > 0 {
		messages = append(users[0].Messages, []Message{
			{
				Role:    "user",
				Message: userMsg,
				Image:   userImage,
			},
			{
				Role:    "model",
				Message: aiMsg,
				Image:   modelImage,
			},
		}...)
		method = "PATCH"
		url = fmt.Sprintf("%susers/%d", mokkyURL, users[0].ID)
	} else {
		messages = []Message{
			{Role: "user", Message: userMsg, Image: userImage},
			{Role: "model", Message: aiMsg, Image: modelImage},
		}
		method = "POST"
		url = mokkyURL + "users"
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
						"Do not use any other special characters or formatting. Respond with the actual content only, no need to add role prefixes."},
				},
			},
			Contents: contextMessages,
			SafetySettings: []Safety{
				{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_NONE"},
				{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_NONE"},
				{Category: "HARM_CATEGORY_SEXUALLY_EXPLICIT", Threshold: "BLOCK_NONE"},
				{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_NONE"},
			},
			Tools: []Tool{
				{
					GoogleSearch: struct{}{},
				},
			},
		}

		jsonData, err := json.Marshal(reqBody)
		if err != nil {
			log.Println("Error marshaling request body:", err)
			return sendChunkedMessage(c, "Error processing your request")
		}

		url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent?key=%s", geminiApiKey)
		log.Printf("Sending request to URL: %s", url)

		client := &http.Client{}
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Println("Error creating request:", err)
			return sendChunkedMessage(c, "Error creating request")
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Println("Error making request to Gemini API:", err)
			return sendChunkedMessage(c, "Error connecting to AI service")
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			log.Printf("Error Response Body: %s\n", body)
			return sendChunkedMessage(c, "Error: API returned non-200 status code")
		}

		rawBody, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Println("Error reading raw response:", err)
			return sendChunkedMessage(c, "Error reading AI response")
		}

		// Parse the response
		var geminiResp GeminiResponse
		if err := json.NewDecoder(bytes.NewReader(rawBody)).Decode(&geminiResp); err != nil {
			log.Println("Error decoding response:", err)
			return sendChunkedMessage(c, "Error decoding AI response")
		}

		if len(geminiResp.Candidates) > 0 && len(geminiResp.Candidates[0].Content.Parts) > 0 {
			responseText := geminiResp.Candidates[0].Content.Parts[0].Text
			telegramID := c.Sender().ID

			// Check if we have grounding sources
			groundingChunks := geminiResp.Candidates[0].GroundingMetadata.GroundingChunks
			if len(groundingChunks) > 0 {
				// Create inline keyboard with source buttons
				var buttons [][]tele.InlineButton
				for i, chunk := range groundingChunks {
					if chunk.Web.Uri != "" {
						title := chunk.Web.Title
						if title == "" {
							title = fmt.Sprintf("Source %d", i+1)
						}
						buttons = append(buttons, []tele.InlineButton{
							{Text: title, URL: chunk.Web.Uri},
						})
					}
				}

				// If we have buttons, send message with inline keyboard
				if len(buttons) > 0 {
					err := c.Send(responseText, &tele.SendOptions{
						ReplyMarkup: &tele.ReplyMarkup{
							InlineKeyboard: buttons,
						},
					})
					if err != nil {
						log.Printf("Error sending response with buttons: %v\n", err)
						// Fallback to regular message if inline keyboard fails
						return sendChunkedMessage(c, responseText)
					}
				} else {
					// No valid buttons, send regular message
					err := sendChunkedMessage(c, responseText)
					if err != nil {
						log.Printf("Error sending response to user: %v\n", err)
						return sendChunkedMessage(c, "Sorry, there was an error sending the response. Please try again.")
					}
				}
			} else {
				// No grounding sources, send regular message
				err := sendChunkedMessage(c, responseText)
				if err != nil {
					log.Printf("Error sending response to user: %v\n", err)
					return sendChunkedMessage(c, "Sorry, there was an error sending the response. Please try again.")
				}
			}

			// Save message after successful sending
			if err := saveMessage(telegramID, userMsg, responseText, c.Sender(), nil, false); err != nil {
				log.Printf("Error saving messages: %v\n", err)
			}
			
			return nil
		}

		return sendChunkedMessage(c, "Sorry, I couldn't generate a response")
	})

	b.Handle(tele.OnPhoto, func(c tele.Context) error {
		photo := c.Message().Photo
		if photo == nil {
			return sendChunkedMessage(c, "No photo found in message")
		}

		c.Notify(tele.Typing)

		file, err := b.File(&photo.File)
		if err != nil {
			log.Printf("Error getting photo file: %v\n", err)
			return sendChunkedMessage(c, "Error processing image")
		}

		data := make([]byte, photo.File.FileSize)
		_, err = file.Read(data)
		if err != nil {
			log.Printf("Error reading photo data: %v\n", err)
			return sendChunkedMessage(c, "Error reading image")
		}

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
			return sendChunkedMessage(c, "Error processing your request")
		}

		url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent?key=%s", geminiApiKey)

		client := &http.Client{}
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Println("Error creating request:", err)
			return sendChunkedMessage(c, "Error creating request")
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Println("Error making request to Gemini API:", err)
			return sendChunkedMessage(c, "Error connecting to AI service")
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			log.Printf("Error Response Body: %s\n", body)
			return sendChunkedMessage(c, "Error: API returned non-200 status code")
		}

		var geminiResp GeminiResponse
		if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
			log.Println("Error decoding response:", err)
			return sendChunkedMessage(c, "Error decoding AI response")
		}

		if len(geminiResp.Candidates) > 0 && len(geminiResp.Candidates[0].Content.Parts) > 0 {
			responseText := geminiResp.Candidates[0].Content.Parts[0].Text
			telegramID := c.Sender().ID
			
			err := sendChunkedMessage(c, responseText)
			if err != nil {
				log.Printf("Error sending response to user: %v\n", err)
				return sendChunkedMessage(c, "Sorry, there was an error sending the response. Please try again.")
			}

			if err := saveMessage(telegramID, userMsg, responseText, c.Sender(), imageData, true); err != nil {
				log.Printf("Error saving messages: %v\n", err)
			}
			
			return nil
		}

		return sendChunkedMessage(c, "Sorry, I couldn't generate a response")
	})

	b.Handle("/history", func(c tele.Context) error {
		c.Notify(tele.Typing)
		err := deleteUserHistory(c.Sender().ID)
		if err != nil {
			log.Printf("Error deleting user history: %v\n", err)
			return sendChunkedMessage(c, "Error deleting user history")
		}
		return sendChunkedMessage(c, "Your messsage history has been cleared!")
	})

	b.Handle("/generate", func(c tele.Context) error {
		prompt := c.Message().Payload
		if prompt == "" {
			return c.Send("Please provide a prompt for image generation. Example: /generate a futuristic cityscape with flying cars")
		}

		c.Notify(tele.Typing)
		log.Printf("Processing image generation request with prompt: %s", prompt)

		// Create request body for image generation
		reqBody := ImageGenerationRequest{
			Contents: []Content{
				{
					Parts: []Part{
						{Text: prompt},
					},
				},
			},
			GenerationConfig: GenerationConfig{
				ResponseModalities: []string{"TEXT", "IMAGE"},
			},
		}

		jsonData, err := json.Marshal(reqBody)
		if err != nil {
			log.Println("Error marshaling request body:", err)
			return c.Send("Error processing your request")
		}

		url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=%s", geminiApiKey)
		log.Printf("Sending request to URL: %s", url)

		client := &http.Client{Timeout: 60 * time.Second}
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
			return c.Send(fmt.Sprintf("Error: API returned status code %d", resp.StatusCode))
		}

		// Read full response body
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Error reading response body: %v", err)
			return c.Send("Error reading API response")
		}

		// Parse the response to extract image data
		var response struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						InlineData struct {
							Data string `json:"data"`
						} `json:"inlineData"`
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}

		if err := json.Unmarshal(responseBody, &response); err != nil {
			log.Printf("Error parsing response JSON: %v", err)
			return c.Send("Error processing the generated image")
		}

		if len(response.Candidates) == 0 || len(response.Candidates[0].Content.Parts) == 0 {
			return c.Send("No image was generated. Please try again with a different prompt.")
		}

		// Find the image data in the response
		var base64Data string
		var caption string
		for _, part := range response.Candidates[0].Content.Parts {
			if part.InlineData.Data != "" {
				base64Data = part.InlineData.Data
			}
			if part.Text != "" {
				caption = part.Text
			}
		}

		if base64Data == "" {
			return c.Send("No image data received. Please try again.")
		}

		// Create FileData structure to save in database
		imageData := &FileData{
			MimeType: "image/png",
			Data:     base64Data,
		}

		// Decode the base64 data for sending via Telegram
		decodedImageData, err := base64.StdEncoding.DecodeString(base64Data)
		if err != nil {
			log.Printf("Error decoding base64 image data: %v", err)
			return c.Send("Error processing the generated image")
		}

		log.Printf("Successfully decoded image data, size: %d bytes", len(decodedImageData))

		// Save the image to a temporary file
		tempFile, err := os.CreateTemp("", "gemini-image-*.png")
		if err != nil {
			log.Printf("Error creating temp file: %v", err)
			return c.Send("Error saving the generated image")
		}

		tempFileName := tempFile.Name()
		defer os.Remove(tempFileName) // Clean up the file when done

		// Write the image data to the file
		if _, err := tempFile.Write(decodedImageData); err != nil {
			log.Printf("Error writing to temp file: %v", err)
			tempFile.Close()
			return c.Send("Error saving the generated image")
		}
		tempFile.Close()

		log.Printf("Image saved to temporary file: %s", tempFileName)

		// Send the image file to the user
		photo := &tele.Photo{File: tele.FromDisk(tempFileName)}

		// Add caption if there's text
		if caption != "" {
			photo.Caption = caption
		}

		// Save the message and image to the database
		telegramID := c.Sender().ID
		if err := saveMessage(telegramID, prompt, caption, c.Sender(), imageData, false); err != nil {
			log.Printf("Error saving generated image to database: %v\n", err)
			// Continue even if saving fails
		} else {
			log.Printf("Successfully saved generated image to user history")
		}

		err = c.Send(photo)
		if err != nil {
			log.Printf("Error sending photo: %v", err)
			return c.Send("Generated an image but couldn't send it. Please try again.")
		}

		log.Printf("Successfully sent image to user")
		return nil
	})

	log.Println("Bot is running...")
	b.Start()
}
