package logfunction

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

func LogDocumentMessageSQS(senderPhone string, text string, recipientPhone string, filePath string, messageTime time.Time, adminPhone string, msgId string, parMsgId string) error {
	if err := godotenv.Load(); err != nil {
		return fmt.Errorf("error loading .env file: %w", err)
	}
	bearerToken := os.Getenv("BEARER_TOKEN")
	if bearerToken == "" {
		return fmt.Errorf("BEARER_TOKEN not set in .env file")
	}

	// Fetch the document from S3 with a timeout
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(filePath)
	if err != nil {
		log.Printf("❌ Error fetching S3 file for message %s: %v", msgId, err)
		return fmt.Errorf("error fetching S3 file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("❌ Non-OK response fetching S3 file for message %s: %s", msgId, resp.Status)
		return fmt.Errorf("non-OK response fetching S3 file: %s", resp.Status)
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	_ = writer.WriteField("entity_phone_number_from", senderPhone)
	_ = writer.WriteField("entity_phone_number_to", recipientPhone)
	_ = writer.WriteField("message_text", text)
	_ = writer.WriteField("message_status", "READ")
	_ = writer.WriteField("message_time", strconv.FormatInt(messageTime.UnixMilli(), 10))
	_ = writer.WriteField("admin_phone", adminPhone)
	_ = writer.WriteField("wa_message_id", msgId)
	_ = writer.WriteField("wa_parent_message_id", parMsgId)

	part, err := writer.CreateFormFile("files", filepath.Base(filePath))
	if err != nil {
		return fmt.Errorf("error creating form file: %w", err)
	}

	_, err = io.Copy(part, resp.Body)
	if err != nil {
		return fmt.Errorf("error copying file data: %w", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("error closing multipart writer: %w", err)
	}

	req, err := http.NewRequest("POST", LogAPIEndpoint, body)
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	clientHTTP := &http.Client{
		Timeout: 15 * time.Second, // Add a timeout to prevent hanging
	}
	apiResp, err := clientHTTP.Do(req)
	if err != nil {
		log.Printf("❌ Error sending document log for message %s: %v", msgId, err)
		return fmt.Errorf("error sending log: %w", err)
	}
	defer apiResp.Body.Close()

	if apiResp.StatusCode != http.StatusOK {
		log.Printf("❌ Error response from log API for document message %s: %s", msgId, apiResp.Status)
		return fmt.Errorf("error response from log API: %s", apiResp.Status)
	}

	log.Printf("✅ Successfully logged document message %s", msgId)
	return nil
}
