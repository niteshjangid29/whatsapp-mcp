package logfunction

import (
	"bytes"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

const LogAPIEndpoint = "http://privatebackend.railse.com:8080/whatsapp/log-message"

func LogMessage(senderPhone string, text string, recipientPhone string, messageTime time.Time, adminPhone string, msgId string, parMsgId string) error {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// load .env file
	if err := godotenv.Load(); err != nil {
		return fmt.Errorf("error loading .env file: %w", err)
	}
	// get BEARER_TOKEN from .env file
	bearerToken := os.Getenv("BEARER_TOKEN")
	if bearerToken == "" {
		return fmt.Errorf("BEARER_TOKEN not set in .env file")
	}

	_ = writer.WriteField("entity_phone_number_from", senderPhone)
	_ = writer.WriteField("entity_phone_number_to", recipientPhone)
	_ = writer.WriteField("message_text", text)
	_ = writer.WriteField("message_status", "READ")
	_ = writer.WriteField("message_time", strconv.FormatInt(messageTime.UnixMilli(), 10))
	_ = writer.WriteField("admin_phone", adminPhone)
	_ = writer.WriteField("wa_message_id", msgId)
	_ = writer.WriteField("wa_parent_message_id", parMsgId)

	if err := writer.Close(); err != nil {
		return fmt.Errorf("error closing multipart writer: %w", err)
	}

	req, err := http.NewRequest("POST", LogAPIEndpoint, body)
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	client := &http.Client{
		Timeout: 15 * time.Second, // Add a timeout to prevent hanging
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("❌ Error sending log for message %s: %v", msgId, err)
		return fmt.Errorf("error sending log: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("❌ Error response from log API for message %s: %s", msgId, resp.Status)
		return fmt.Errorf("error response from log API: %s", resp.Status)
	}

	log.Printf("✅ Successfully logged message %s", msgId)
	return nil
}
