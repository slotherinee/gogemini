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
	"regexp"
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

type GenerationConfig struct {
	ResponseModalities []string `json:"responseModalities"`
}

type ImageGenerationRequest struct {
	Contents         []Content        `json:"contents"`
	GenerationConfig GenerationConfig `json:"generationConfig"`
	SafetySettings   []Safety         `json:"safety_settings,omitempty"`
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
			if err := saveMessage(telegramID, userMsg, responseText, c.Sender(), nil, false); err != nil {
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
			if err := saveMessage(telegramID, userMsg, responseText, c.Sender(), imageData, true); err != nil {
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
				ResponseModalities: []string{"Text", "Image"},
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

		url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash-exp-image-generation:generateContent?key=%s", geminiApiKey)
		log.Printf("Sending request to URL: %s", url)

		client := &http.Client{Timeout: 60 * time.Second} // Longer timeout for image generation
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

		// Extract base64 image data directly with regex
		log.Printf("Extracting image data from response")

		// Use regex to find the base64 encoded image data
		re := regexp.MustCompile(`"data"\s*:\s*"([^"]+)"`)
		matches := re.FindStringSubmatch(string(responseBody))

		if len(matches) < 2 {
			log.Printf("No image data found in the response")
			return c.Send("Sorry, couldn't generate an image. Please try with a different prompt.")
		}

		base64Data := matches[1]
		log.Printf("Found base64 image data of length: %d", len(base64Data))

		// Create FileData structure to save in database
		imageData := &FileData{
			MimeType: "image/png",
			Data:     base64Data,
		}

		// Extract any text from the response (if present)
		reText := regexp.MustCompile(`"text"\s*:\s*"([^"]*)"`)
		textMatches := reText.FindStringSubmatch(string(responseBody))

		var responseText string
		if len(textMatches) >= 2 && textMatches[1] != "" {
			responseText = textMatches[1]
			log.Printf("Found text to use as caption: %s", textMatches[1])
		} else {
			responseText = "Generated image based on your prompt."
		}

		// Save the message and image to the database
		telegramID := c.Sender().ID
		if err := saveMessage(telegramID, prompt, responseText, c.Sender(), imageData, false); err != nil {
			log.Printf("Error saving generated image to database: %v\n", err)
			// Continue even if saving fails
		} else {
			log.Printf("Successfully saved generated image to user history")
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
		if responseText != "" {
			photo.Caption = responseText
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
