package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Message represents a chat message for our client
type Message struct {
	Time     time.Time
	Sender   string
	Content  string
	IsFromMe bool
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0755); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages
	db, err := sql.Open("sqlite3", "file:store/messages.db?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	return &MessageStore{db: db}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool) error {
	// Only store if there's actual content
	if content == "" {
		return nil
	}

	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, ?)",
		id, chatJID, sender, content, timestamp, isFromMe,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// For now, we're ignoring non-text messages
	return ""
}

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
}

type SendMessageResponseWithLog struct {
	Success       bool   `json:"success"`
	Message       string `json:"message"`
	MessageLogged string `json:"message_logged"`
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, recipient string, message string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	// Send the message
	_, err = client.SendMessage(context.Background(), recipientJID, &waProto.Message{
		Conversation: proto.String(message),
	})

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	return true, fmt.Sprintf("Message sent to %s", recipient)
}

func sendWhatsAppImageMessage(client *whatsmeow.Client, recipient string, message string, image []byte) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	resp, err := client.Upload(context.Background(), image, whatsmeow.MediaImage)
	// handle error
	if err != nil {
		return false, fmt.Sprintf("Error uploading image: %v", err)
	}

	imageMsg := &waE2E.ImageMessage{
		Caption:  proto.String(message),
		Mimetype: proto.String("image/png"), // replace this with the actual mime type
		// you can also optionally add other fields like ContextInfo and JpegThumbnail here

		URL:           &resp.URL,
		DirectPath:    &resp.DirectPath,
		MediaKey:      resp.MediaKey,
		FileEncSHA256: resp.FileEncSHA256,
		FileSHA256:    resp.FileSHA256,
		FileLength:    &resp.FileLength,
	}
	_, err = client.SendMessage(context.Background(), recipientJID, &waE2E.Message{
		ImageMessage: imageMsg,
	})

	if err != nil {
		return false, fmt.Sprintf("Error sending image message: %v", err)
	}

	return true, fmt.Sprintf("Image message sent to %s", recipient)
}

func sendWhatsAppDocumentMessage(client *whatsmeow.Client, recipient string, message string, document []byte, fileName string, mimeType string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	resp, err := client.Upload(context.Background(), document, whatsmeow.MediaDocument)
	if err != nil {
		return false, fmt.Sprintf("Error uploading document: %v", err)
	}

	docMsg := &waE2E.DocumentMessage{
		Title:         proto.String(fileName),
		FileName:      proto.String(fileName),
		Mimetype:      proto.String(mimeType),
		Caption:       proto.String(message),
		URL:           &resp.URL,
		DirectPath:    &resp.DirectPath,
		MediaKey:      resp.MediaKey,
		FileSHA256:    resp.FileSHA256,
		FileEncSHA256: resp.FileEncSHA256,
		FileLength:    &resp.FileLength,
	}

	_, err = client.SendMessage(context.Background(), recipientJID, &waE2E.Message{
		DocumentMessage: docMsg,
	})

	if err != nil {
		return false, fmt.Sprintf("Error sending document message: %v", err)
	}

	return true, fmt.Sprintf("Document message sent to %s", recipient)
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, port int) {
	// Handler for sending messages
	http.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		fmt.Println("Received request to send message")
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse request body
		var req SendMessageRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			fmt.Println("Error parsing request body:", err)
			http.Error(w, "Error parsing request body", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" || req.Message == "" {
			http.Error(w, "Recipient and message are required", http.StatusBadRequest)
			return
		}

		// Send the message
		success, msg := sendWhatsAppMessage(client, req.Recipient, req.Message)
		fmt.Println("Message sent", success, msg)

		var messageLogged string
		// Log the message
		if success {
			senderPhone := client.Store.ID.User
			recipientPhone := req.Recipient
			msgTime := time.Now()
			err := logMessage(senderPhone, req.Message, recipientPhone, msgTime)
			if err != nil {
				fmt.Println("⚠️ Failed to log message:", err)
				messageLogged = "Failed to log message"
			} else {
				fmt.Println("✅ Message logged successfully")
				messageLogged = "Message logged successfully"
			}
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		json.NewEncoder(w).Encode(SendMessageResponseWithLog{
			Success:       success,
			Message:       msg,
			MessageLogged: messageLogged,
		})
	})

	http.HandleFunc("/api/send-image", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("Received request to send message")
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		file, _, err := r.FormFile("file")
		// fileName := header.Filename
		// mimeType := header.Header.Get("Content-Type")
		if err != nil {
			fmt.Println("Error retrieving file:", err)
			http.Error(w, "Error retrieving file", http.StatusBadRequest)
			return
		}
		defer file.Close()

		// Get additional form fields
		recipient := r.FormValue("recipient")
		message := r.FormValue("message")

		// Read the file into a byte array
		fileBytes, err := io.ReadAll(file)
		if err != nil {
			fmt.Println("Error reading file:", err)
			http.Error(w, "Error reading file", http.StatusInternalServerError)
			return
		}

		// Validate request
		if recipient == "" || message == "" {
			http.Error(w, "Recipient and message are required", http.StatusBadRequest)
			return
		}

		// Save the file temporarily
		tmpFile := fmt.Sprintf("store/image_%d.jpg", time.Now().UnixNano())
		err = os.WriteFile(tmpFile, fileBytes, 0644)
		if err != nil {
			fmt.Println("Error saving file:", err)
			http.Error(w, "Error saving file", http.StatusInternalServerError)
			return
		}
		defer os.Remove(tmpFile)

		// Send the message
		success, msg := sendWhatsAppImageMessage(client, recipient, message, fileBytes)
		fmt.Println("Message sent", success, msg)

		var messageLogged string
		// Log the message
		if success {
			senderPhone := client.Store.ID.User
			recipientPhone := recipient
			msgTime := time.Now()
			err := logImageMessage(senderPhone, message, recipientPhone, tmpFile, msgTime)
			if err != nil {
				fmt.Println("⚠️ Failed to log message:", err)
				messageLogged = "Failed to log message"
			} else {
				fmt.Println("✅ Message logged successfully")
				messageLogged = "Message logged successfully"
			}
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		json.NewEncoder(w).Encode(SendMessageResponseWithLog{
			Success:       success,
			Message:       msg,
			MessageLogged: messageLogged,
		})
	})

	http.HandleFunc("/api/send-document", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("Received request to send document message")
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		file, header, err := r.FormFile("file")
		fileName := header.Filename
		mimeType := header.Header.Get("Content-Type")
		if err != nil {
			fmt.Println("Error retrieving file:", err)
			http.Error(w, "Error retrieving file", http.StatusBadRequest)
			return
		}
		defer file.Close()

		// Get additional form fields
		recipient := r.FormValue("recipient")
		message := r.FormValue("message")

		// Read the file into a byte array
		fileBytes, err := io.ReadAll(file)
		if err != nil {
			fmt.Println("Error reading file:", err)
			http.Error(w, "Error reading file", http.StatusInternalServerError)
			return
		}

		// Validate request
		if recipient == "" || message == "" {
			http.Error(w, "Recipient and message are required", http.StatusBadRequest)
			return
		}

		// Save the file temporarily
		tmpFile := fmt.Sprintf("store/document_%d.pdf", time.Now().UnixNano())
		err = os.WriteFile(tmpFile, fileBytes, 0644)
		if err != nil {
			fmt.Println("Error saving file:", err)
			http.Error(w, "Error saving file", http.StatusInternalServerError)
			return
		}
		defer os.Remove(tmpFile)

		// Send the message
		success, msg := sendWhatsAppDocumentMessage(client, recipient, message, fileBytes, fileName, mimeType)
		fmt.Println("Message sent", success, msg)

		var messageLogged string
		// Log the message
		if success {
			senderPhone := client.Store.ID.User
			recipientPhone := recipient
			msgTime := time.Now()
			err := logDocumentMessage(senderPhone, message, recipientPhone, tmpFile, msgTime)
			if err != nil {
				fmt.Println("⚠️ Failed to log message:", err)
				messageLogged = "Failed to log message"
			} else {
				fmt.Println("✅ Message logged successfully")
				messageLogged = "Message logged successfully"
			}
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")
		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		json.NewEncoder(w).Encode(SendMessageResponseWithLog{
			Success:       success,
			Message:       msg,
			MessageLogged: messageLogged,
		})
	})

	// Start the server
	serverAddr := fmt.Sprintf(":%d", port)
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := http.ListenAndServe(serverAddr, nil); err != nil {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
}

const LogAPIEndpoint = "https://backend.railse.com/whatsapp/log-message"

func logMessage(senderPhone string, text string, recipientPhone string, messageTime time.Time) error {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// load .env file
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	// get BEARER_TOKEN from .env file
	bearerToken := os.Getenv("BEARER_TOKEN")
	if bearerToken == "" {
		log.Fatal("BEARER_TOKEN not set in .env file")
	}

	_ = writer.WriteField("entity_phone_number_from", senderPhone)
	_ = writer.WriteField("entity_phone_number_to", recipientPhone)
	_ = writer.WriteField("message_text", text)
	_ = writer.WriteField("message_status", "READ")
	_ = writer.WriteField("message_time", strconv.FormatInt(messageTime.UnixMilli(), 10))

	writer.Close()

	req, err := http.NewRequest("POST", LogAPIEndpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	log.Println("📤 REQUEST", req)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Println("❌ Error sending log:", err)
		return err
	}
	defer resp.Body.Close()

	return nil
}

func logImageMessage(senderPhone string, text string, recipientPhone string, filePath string, messageTime time.Time) error {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	bearerToken := os.Getenv("BEARER_TOKEN")
	if bearerToken == "" {
		log.Fatal("BEARER_TOKEN not set in .env file")
	}

	file, err := os.Open(filePath)
	if err != nil {
		log.Println("❌ Error opening file:", err)
		return err
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	_ = writer.WriteField("entity_phone_number_from", senderPhone)
	_ = writer.WriteField("entity_phone_number_to", recipientPhone)
	_ = writer.WriteField("message_text", text)
	_ = writer.WriteField("message_status", "READ")
	_ = writer.WriteField("message_time", strconv.FormatInt(messageTime.UnixMilli(), 10))

	part, err := writer.CreateFormFile("files", filepath.Base(filePath))
	if err != nil {
		log.Println("❌ Error creating form file:", err)
		return err
	}

	_, err = io.Copy(part, file)
	if err != nil {
		log.Println("❌ Error copying file data:", err)
		return err
	}

	writer.Close()

	req, err := http.NewRequest("POST", LogAPIEndpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	log.Println("📤 Image REQUEST", req)

	clientHTTP := &http.Client{}
	resp, err := clientHTTP.Do(req)
	if err != nil {
		log.Println("❌ Error sending log:", err)
		return err
	}
	defer resp.Body.Close()

	log.Println("✅ Image message logged successfully")
	return nil
}

func logDocumentMessage(senderPhone string, text string, recipientPhone string, filePath string, messageTime time.Time) error {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	bearerToken := os.Getenv("BEARER_TOKEN")
	if bearerToken == "" {
		log.Fatal("BEARER_TOKEN not set in .env file")
	}
	file, err := os.Open(filePath)
	if err != nil {
		log.Println("❌ Error opening file:", err)
		return err
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	_ = writer.WriteField("entity_phone_number_from", senderPhone)
	_ = writer.WriteField("entity_phone_number_to", recipientPhone)
	_ = writer.WriteField("message_text", text)
	_ = writer.WriteField("message_status", "READ")
	_ = writer.WriteField("message_time", strconv.FormatInt(messageTime.UnixMilli(), 10))
	part, err := writer.CreateFormFile("files", filepath.Base(filePath))
	if err != nil {
		log.Println("❌ Error creating form file:", err)
		return err
	}

	_, err = io.Copy(part, file)
	if err != nil {
		log.Println("❌ Error copying file data:", err)
		return err
	}

	writer.Close()

	req, err := http.NewRequest("POST", LogAPIEndpoint, body)
	if err != nil {
		log.Println("❌ Error creating request:", err)
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	log.Println("📤 Document REQUEST", req)

	clientHTTP := &http.Client{}
	resp, err := clientHTTP.Do(req)
	if err != nil {
		log.Println("❌ Error sending log:", err)
		return err
	}
	defer resp.Body.Close()

	log.Println("✅ Document message logged successfully")
	return nil
}

func main() {
	// Set up logger
	logger := waLog.Stdout("Client", "DEBUG", true)
	logger.Infof("Starting WhatsApp client...")

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0755); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	container, err := sqlstore.New("sqlite3", "file:store/whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, v, logger)
			sender := v.Info.Sender.User
			timestamp := v.Info.Timestamp
			text := v.Message.GetConversation()
			recipient := v.Info.Chat.User
			image := v.Message.ImageMessage
			document := v.Message.DocumentMessage

			// Do not save status messages
			if sender == "status" || recipient == "status" || sender == "status@broadcast" || recipient == "status@broadcast" {
				return
			}

			// Check if the message is a document
			if document != nil {
				data, err := client.Download(v.Message.DocumentMessage)
				if err != nil {
					logger.Errorf("❌ Failed to download document: %v", err)
					return
				}

				// Save document temporarily
				tmpFile := fmt.Sprintf("store/document_%d.pdf", time.Now().UnixNano())
				err = os.WriteFile(tmpFile, data, 0644)
				if err != nil {
					logger.Errorf("❌ Failed to save document: %v", err)
					return
				}
				defer os.Remove(tmpFile)

				sender := v.Info.Sender.User
				recipient := v.Info.Chat.User
				timestamp := v.Info.Timestamp
				caption := ""
				if v.Message.DocumentMessage.Caption != nil {
					caption = *v.Message.DocumentMessage.Caption
				}
				err = logDocumentMessage(sender, caption, recipient, tmpFile, timestamp)
				if err != nil {
					logger.Errorf("❌ Failed to log document message: %v", err)
				} else {
					logger.Infof("✅ Document message logged successfully")
				}
				fmt.Printf("📥 Received document from %s to %s: %s\n", sender, recipient, caption)
			}

			if image != nil {
				data, err := client.Download(v.Message.ImageMessage)
				if err != nil {
					logger.Errorf("❌ Failed to download image: %v", err)
					return
				}

				// Save image temporarily
				tmpFile := fmt.Sprintf("store/image_%d.jpg", time.Now().UnixNano())
				err = os.WriteFile(tmpFile, data, 0644)
				if err != nil {
					logger.Errorf("❌ Failed to save image: %v", err)
					return
				}
				defer os.Remove(tmpFile)

				sender := v.Info.Sender.User
				recipient := v.Info.Chat.User
				timestamp := v.Info.Timestamp
				caption := ""
				if v.Message.ImageMessage.Caption != nil {
					caption = *v.Message.ImageMessage.Caption
				}
				err = logImageMessage(sender, caption, recipient, tmpFile, timestamp)
				if err != nil {
					logger.Errorf("❌ Failed to log image message: %v", err)
				} else {
					logger.Infof("✅ Image message logged successfully")
				}
				fmt.Printf("📥 Received image from %s to %s: %s\n", sender, recipient, caption)
			}

			if text != "" {
				fmt.Printf("📥 Received from %s to %s: %s\n", sender, recipient, text)
				err := logMessage(sender, text, recipient, timestamp)
				if err != nil {
					logger.Errorf("❌ Failed to log message: %v", err)
				} else {
					logger.Infof("✅ Message logged successfully")
				}
			}

		case *events.Receipt:
			// Process regular messages
			handleReceipt(client, messageStore, v, logger)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger)

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
		}
	})

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Print QR code for pairing with phone
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp! Type 'help' for commands.")

	// Start REST API server
	startRESTServer(client, 6000)

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// Disconnect client
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" {
		// Chat exists with a name, use that
		logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		logger.Infof("Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Infof("Using group name: %s", name)
	} else {
		// This is an individual contact
		logger.Infof("Getting name for contact: %s", chatJID)

		// Just use contact info (full name)
		contact, err := client.Store.Contacts.GetContact(jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			// Fallback to sender
			name = sender
		} else {
			// Last fallback to JID
			name = jid.User
		}

		logger.Infof("Using contact name: %s", name)
	}

	return name
}

// Handle regular incoming messages
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Extract text content
	content := extractTextContent(msg.Message)
	if content == "" {
		return // Skip non-text messages
	}

	// Save message to database
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
	)
	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}
		fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
	}
}

func handleReceipt(client *whatsmeow.Client, messageStore *MessageStore, receipt *events.Receipt, logger waLog.Logger) {
	logger.Infof("receipt %v", receipt)
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// Log the message content for debugging
				logger.Infof("Message content: %v", content)

				// Skip non-text messages
				if content == "" {
					continue
				}

				// Determine sender
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						sender = *msg.Message.Key.Participant
					} else if isFromMe {
						sender = client.Store.ID.User
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					// Log successful message storage
					logger.Infof("Stored message: [%s] %s -> %s: %s", timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d text messages.\n", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		fmt.Println("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		fmt.Println("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
		fmt.Println("Client is not logged in. Please scan the QR code first.")
		return
	}

	// Build and send a history sync request
	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		fmt.Println("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		fmt.Printf("Failed to request history sync: %v\n", err)
	} else {
		fmt.Println("History sync requested. Waiting for server response...")
	}
}
