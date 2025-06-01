package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"
	logfunction "whatsapp-client/log-function"

	"go.mau.fi/libsignal/logger"
	"go.mau.fi/whatsmeow/proto/waE2E"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
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

type CreateGroupRequest struct {
	GroupName string   `json:"group_name"`
	Members   []string `json:"members"`
}

type CreateGroupResponse struct {
	Success  bool   `json:"success"`
	GroupJID string `json:"group_jid"`
	Message  string `json:"message"`
}

type GroupInfo struct {
	Name        string `json:"name"`
	JID         string `json:"jid"`
	CreatedTime int64  `json:"created_time"`
}

func uploadToS3(bucketName string, key string, data []byte) (string, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(os.Getenv("AWS_REGION")))
	if err != nil {
		return "", err
	}

	s3Client := s3.NewFromConfig(cfg)

	contentType := http.DetectContentType(data)

	_, err = s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      aws.String(bucketName),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", err
	}

	url := "https://" + bucketName + ".s3." + os.Getenv("AWS_REGION") + ".amazonaws.com/" + key
	return url, nil
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
	Success bool   `json:"success"`
	Message string `json:"message"`
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
		server := "s.whatsapp.net" // Default server for personal chats
		if strings.Contains(recipient, "-") {
			server = "g.us" // Group chats use g.us
		}

		recipientJID = types.JID{
			User:   recipient,
			Server: server,
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
		server := "s.whatsapp.net" // Default server for personal chats
		if strings.Contains(recipient, "-") {
			server = "g.us" // Group chats use g.us
		}
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: server,
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
		server := "s.whatsapp.net" // Default server for personal chats
		if strings.Contains(recipient, "-") {
			server = "g.us" // Group chats use g.us
		}
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: server,
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

func createWhatsAppGroup(client *whatsmeow.Client, req CreateGroupRequest) (CreateGroupResponse, error) {
	if !client.IsConnected() {
		return CreateGroupResponse{
			Success: false,
			Message: "Not connected to WhatsApp",
		}, nil
	}

	if strings.TrimSpace(req.GroupName) == "" || len(req.Members) == 0 {
		return CreateGroupResponse{
			Success: false,
			Message: "Group name and at least one member are required",
		}, nil
	}

	if len(req.GroupName) > 25 {
		return CreateGroupResponse{
			Success: false,
			Message: "Group name exceeds 25 character limit",
		}, nil
	}

	var memberJIDs []types.JID
	for _, member := range req.Members {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}

		var memberJID types.JID
		var err error
		if strings.Contains(member, "@") {
			memberJID, err = types.ParseJID(member)
			if err != nil {
				return CreateGroupResponse{
					Success: false,
					Message: fmt.Sprintf("Invalid JID '%s': %v", member, err),
				}, nil
			}
		} else {
			member = strings.NewReplacer("+", "", "-", "", " ", "").Replace(member)
			memberJID = types.NewJID(member, types.DefaultUserServer)
		}
		memberJIDs = append(memberJIDs, memberJID)
	}

	createReq := whatsmeow.ReqCreateGroup{
		Name:         req.GroupName,
		Participants: memberJIDs,
		CreateKey:    client.GenerateMessageID(),
	}

	groupInfo, err := client.CreateGroup(createReq)
	if err != nil {
		return CreateGroupResponse{
			Success: false,
			Message: fmt.Sprintf("Error creating group: %v", err),
		}, nil
	}

	return CreateGroupResponse{
		Success:  true,
		GroupJID: groupInfo.JID.String(),
		Message:  "Group created successfully",
	}, nil
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, sqsClient *sqs.Client, queueURL string, port int) {
	// Handler for creating a group
	http.HandleFunc("/api/create-group", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("Received request to create group")
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()

		// Parse JSON body
		var req CreateGroupRequest
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, "Invalid request payload: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Call createWhatsAppGroup function
		resp, err := createWhatsAppGroup(client, req)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to create group: %v", err), http.StatusInternalServerError)
			return
		}

		// Set response header and write JSON response
		if resp.Success {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(resp)
	})

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

		// Log the message
		if success {
			senderPhone := client.Store.ID.User
			recipientPhone := req.Recipient
			msgTime := time.Now()
			// err := logfunction.LogMessage(senderPhone, req.Message, recipientPhone, msgTime)
			// if err != nil {
			// 	fmt.Println("⚠️ Failed to log message:", err)
			// 	messageLogged = "Failed to log message"
			// } else {
			// 	fmt.Println("✅ Message logged successfully")
			// 	messageLogged = "Message logged successfully"
			// }
			err := sendMessageToQueue(WALogMessageForQueue{
				Type:    "text",
				From:    senderPhone,
				To:      recipientPhone,
				Message: req.Message,
				File:    "",
				Time:    msgTime,
			}, sqsClient, queueURL)
			if err != nil {
				logger.Error("Failed to send message to SQS:", err)
			} else {
				logger.Info("Message sent to SQS successfully")
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
			Success: success,
			Message: msg,
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
		// tmpFile := fmt.Sprintf("whatsapp_failed_files/image_%d.jpg", time.Now().UnixNano())
		// err = os.WriteFile(tmpFile, fileBytes, 0644)
		// if err != nil {
		// 	fmt.Println("Error saving file:", err)
		// 	http.Error(w, "Error saving file", http.StatusInternalServerError)
		// 	return
		// }
		// defer os.Remove(tmpFile)

		// Send the message
		success, msg := sendWhatsAppImageMessage(client, recipient, message, fileBytes)
		fmt.Println("Message sent", success, msg)

		// Log the message
		if success {
			senderPhone := client.Store.ID.User
			recipientPhone := recipient
			msgTime := time.Now()
			// err := logfunction.LogImageMessage(senderPhone, message, recipientPhone, tmpFile, msgTime)
			// if err != nil {
			// 	fmt.Println("⚠️ Failed to log message:", err)
			// 	messageLogged = "Failed to log message"
			// } else {
			// 	fmt.Println("✅ Message logged successfully")
			// 	messageLogged = "Message logged successfully"
			// }

			tmpFile := fmt.Sprintf("whatsapp_failed_files/image_%d.jpg", time.Now().UnixNano())
			url, err := uploadToS3(os.Getenv("AWS_S3_BUCKET_NAME"), tmpFile, fileBytes)
			if err != nil {
				fmt.Println("Error uploading file to S3:", err)
				http.Error(w, "Error uploading file to S3", http.StatusInternalServerError)
				return
			} else {
				err = sendMessageToQueue(WALogMessageForQueue{
					Type:    "image",
					From:    senderPhone,
					To:      recipientPhone,
					Message: message,
					Time:    msgTime,
					File:    url,
				}, sqsClient, queueURL)
				if err != nil {
					logger.Error("⚠️ Failed to send message to SQS:", err)
				} else {
					logger.Info("✅ Message sent to SQS successfully")
				}
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
			Success: success,
			Message: msg,
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
		// tmpFile := fmt.Sprintf("store/document_%d.pdf", time.Now().UnixNano())
		// err = os.WriteFile(tmpFile, fileBytes, 0644)
		// if err != nil {
		// 	fmt.Println("Error saving file:", err)
		// 	http.Error(w, "Error saving file", http.StatusInternalServerError)
		// 	return
		// }
		// defer os.Remove(tmpFile)

		// Send the message
		success, msg := sendWhatsAppDocumentMessage(client, recipient, message, fileBytes, fileName, mimeType)
		fmt.Println("Message sent", success, msg)

		// Log the message
		if success {
			senderPhone := client.Store.ID.User
			recipientPhone := recipient
			msgTime := time.Now()

			tmpFile := fmt.Sprintf("whatsapp_failed_files/document_%d.pdf", time.Now().UnixNano())
			url, err := uploadToS3(os.Getenv("AWS_S3_BUCKET_NAME"), tmpFile, fileBytes)
			if err != nil {
				fmt.Println("Error uploading file to S3:", err)
				http.Error(w, "Error uploading file to S3", http.StatusInternalServerError)
				return
			} else {
				err = sendMessageToQueue(WALogMessageForQueue{
					Type:    "document",
					From:    senderPhone,
					To:      recipientPhone,
					Message: message,
					File:    url,
					Time:    msgTime,
				}, sqsClient, queueURL)
				if err != nil {
					logger.Error("⚠️ Failed to send message to SQS:", err)
				} else {
					logger.Info("✅ Message sent to SQS successfully")
				}
			}
			// err = logfunction.LogDocumentMessage(senderPhone, message, recipientPhone, url, msgTime)
			// if err != nil {
			// 	fmt.Println("⚠️ Failed to log message:", err)
			// } else {
			// 	fmt.Println("✅ Message logged successfully")
			// }
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")
		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		json.NewEncoder(w).Encode(SendMessageResponseWithLog{
			Success: success,
			Message: msg,
		})
	})

	http.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("Received request for group info")
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if !client.IsConnected() {
			http.Error(w, "Not connected to WhatsApp", http.StatusInternalServerError)
			return
		}

		// Get all groups
		groups, err := client.GetJoinedGroups()
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get groups: %v", err), http.StatusInternalServerError)
			return
		}

		var groupList []GroupInfo
		for _, group := range groups {
			groupList = append(groupList, GroupInfo{
				Name:        group.Name,
				JID:         group.JID.String(),
				CreatedTime: group.GroupCreated.UnixMilli(),
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(groupList)
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

type WALogMessageForQueue struct {
	Type    string    `json:"type"` // "text", "image", "document"
	From    string    `json:"from"`
	To      string    `json:"to"`
	Message string    `json:"message"`
	File    string    `json:"file"`
	Time    time.Time `json:"time"`
}

func sendMessageToQueue(message WALogMessageForQueue, sqsClient *sqs.Client, queueUrl string) error {
	messageBytes, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("error marshalling message: %w", err)
	}

	_, err = sqsClient.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl:    aws.String(queueUrl),
		MessageBody: aws.String(string(messageBytes)),
	})
	if err != nil {
		return fmt.Errorf("error sending message to SQS: %w", err)
	}
	fmt.Println("✅ Message sent to SQS queue successfully")
	return nil
}

func recieveMessagesFromQueue(sqsClient *sqs.Client, queueUrl string) error {
	output, err := sqsClient.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(queueUrl),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     5,
	})
	if err != nil {
		return fmt.Errorf("error receiving message from SQS: %w", err)
	}

	if len(output.Messages) == 0 {
		fmt.Println("No messages in the queue")
		return nil
	}
	fmt.Println("Received", len(output.Messages), "messages from SQS queue")

	for _, msg := range output.Messages {
		var message WALogMessageForQueue
		err := json.Unmarshal([]byte(*msg.Body), &message)
		if err != nil {
			fmt.Println("❌ Error unmarshalling message:", err)
			continue
		}

		var logErr error
		switch message.Type {
		case "text":
			logErr = logfunction.LogMessage(message.From, message.Message, message.To, message.Time)
		case "image":
			logErr = logfunction.LogImageMessageSQS(message.From, message.Message, message.To, message.File, message.Time)
		case "document":
			logErr = logfunction.LogDocumentMessageSQS(message.From, message.Message, message.To, message.File, message.Time)
		default:
			fmt.Println("❌ Unknown message type:", message.Type)
			continue
		}

		if logErr != nil {
			fmt.Println("❌ Error logging message:", logErr)
			continue
		}

		// Delete from queue
		_, err = sqsClient.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
			QueueUrl:      aws.String(queueUrl),
			ReceiptHandle: msg.ReceiptHandle,
		})
		if err != nil {
			return fmt.Errorf("error deleting message from SQS: %w", err)
		}
		fmt.Println("✅ Message processed and deleted from SQS:", message.Message)
	}

	return nil
}

var awsConfig *aws.Config

func getConfig() *aws.Config {
	if awsConfig == nil {
		cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(os.Getenv("AWS_REGION")))
		if err != nil {
			fmt.Println("Error loading AWS config:", err)
			return nil
		}
		awsConfig = &cfg
	}
	return awsConfig
}

func main() {
	// c := cron.New()

	err := godotenv.Load()
	if err != nil {
		fmt.Println("Error loading .env file")
		return
	}

	ctx := context.Background()
	sqsClient := sqs.NewFromConfig(*getConfig())

	// Get Queue URL
	result, err := sqsClient.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{
		QueueName: aws.String(os.Getenv("AWS_SQS_QUEUE_NAME")),
	})
	if err != nil {
		fmt.Println("Error getting SQS queue URL:", err)
		return
	}
	fmt.Println("SQS Queue URL:", *result.QueueUrl)

	// Crone job
	// Start SQS polling in a separate goroutine
	go func() {
		for {
			err = recieveMessagesFromQueue(sqsClient, *result.QueueUrl)
			if err != nil {
				fmt.Println("❌ Error receiving message from SQS:", err)
			} else {
				fmt.Println("✅ Successfully received message from SQS")
			}
			fmt.Println("-------------Cron job executed-------------")
			time.Sleep(10 * time.Second) // Sleep for 10 seconds before next iteration
		}
	}()

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
			var sender, recipient string
			handleMessage(client, messageStore, v, logger)

			// Is group message?
			if v.Info.Chat.Server == "g.us" {
				sender = v.Info.Sender.User      // actual sender inside the group
				recipient = v.Info.Chat.String() // full group JID
			} else {
				if v.Info.MessageSource.IsFromMe {
					// Message from me
					sender = client.Store.ID.User
					recipient = v.Info.Chat.User
				} else {
					// Message to me
					sender = v.Info.Chat.User
					recipient = client.Store.ID.User
				}
			}

			timestamp := v.Info.Timestamp
			text := v.Message.GetConversation()
			image := v.Message.ImageMessage
			document := v.Message.DocumentMessage

			fmt.Println("Received message:", text, "from", sender, "to", recipient)

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
				tmpFile := fmt.Sprintf("whatsapp_failed_files/document_%d.pdf", time.Now().UnixNano())

				// upload to s3
				url, err := uploadToS3(os.Getenv("AWS_S3_BUCKET_NAME"), tmpFile, data)
				if err != nil {
					logger.Errorf("❌ Failed to upload document to S3: %v", err)
					return
				}
				timestamp := v.Info.Timestamp
				caption := ""
				if v.Message.DocumentMessage.Caption != nil {
					caption = *v.Message.DocumentMessage.Caption
				}

				err = sendMessageToQueue(WALogMessageForQueue{
					Type:    "document",
					From:    sender,
					To:      recipient,
					Message: caption,
					Time:    timestamp,
					File:    url,
				}, sqsClient, *result.QueueUrl)
				if err != nil {
					logger.Errorf("❌ Failed to send document message to SQS: %v", err)
					return
				} else {
					logger.Infof("✅ Document message sent to SQS queue successfully")
				}
			}

			if image != nil {
				data, err := client.Download(v.Message.ImageMessage)
				if err != nil {
					logger.Errorf("❌ Failed to download image: %v", err)
					return
				}

				// Save image temporarily
				tmpFile := fmt.Sprintf("whatsapp_failed_files/image_%d.jpg", time.Now().UnixNano())

				// upload to s3
				url, err := uploadToS3(os.Getenv("AWS_S3_BUCKET_NAME"), tmpFile, data)
				// log.Println("URL = ", url)
				if err != nil {
					logger.Errorf("❌ Failed to upload image to S3: %v", err)
					return
				}

				timestamp := v.Info.Timestamp
				caption := ""
				if v.Message.ImageMessage.Caption != nil {
					caption = *v.Message.ImageMessage.Caption
				}

				err = sendMessageToQueue(WALogMessageForQueue{
					Type:    "image",
					From:    sender,
					To:      recipient,
					Message: caption,
					Time:    timestamp,
					File:    url,
				}, sqsClient, *result.QueueUrl)
				if err != nil {
					logger.Errorf("❌ Failed to send image message to SQS: %v", err)
				} else {
					logger.Infof("✅ Image message sent to SQS queue successfully")
				}
			}

			if text != "" {
				fmt.Printf("📥 Received from %s to %s: %s\n", sender, recipient, text)

				// Send message to SQS queue
				err = sendMessageToQueue(WALogMessageForQueue{
					Type:    "text",
					From:    sender,
					To:      recipient,
					Message: text,
					Time:    timestamp,
					File:    "",
				}, sqsClient, *result.QueueUrl)
				if err != nil {
					logger.Errorf("❌ Failed to send message to SQS: %v", err)
				} else {
					logger.Infof("✅ Message sent to SQS queue successfully")
				}
			}

			// print("REPLY Message1: ", v.Message.GetExtendedTextMessage().GetText()) // ye reply message hai
			replyMessage := ""

			if v.Message.GetExtendedTextMessage() != nil && v.Message.GetExtendedTextMessage().GetContextInfo() != nil {
				replyMessage = v.Message.GetExtendedTextMessage().GetText()

				if replyMessage != "" {
					err = sendMessageToQueue(WALogMessageForQueue{
						Type:    "text",
						From:    sender,
						To:      recipient,
						Message: replyMessage,
						Time:    timestamp,
					}, sqsClient, *result.QueueUrl)
					if err != nil {
						logger.Errorf("❌ Failed to send reply message to SQS: %v", err)
					} else {
						logger.Infof("✅ Reply message sent to SQS queue successfully")
					}
				}
			}

			// replyMessage = *v.Message.GetExtendedTextMessage().GetContextInfo().QuotedMessage.Conversation // jiska reply kiya hai
			// println("Reply message2: ", replyMessage)

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
	startRESTServer(client, sqsClient, *result.QueueUrl, 6000)

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
