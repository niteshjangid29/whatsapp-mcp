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

const LogAPIEndpoint = "https://backend.railse.com/whatsapp/log-message"

func LogMessage(senderPhone string, text string, recipientPhone string, messageTime time.Time, adminPhone string, msgId string, parMsgId string) error {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// load .env file
	err := godotenv.Load()
	if err != nil {
		return err
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

	writer.Close()

	req, err := http.NewRequest("POST", LogAPIEndpoint, body)
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	// log.Println("📤 REQUEST", req)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Println("❌ Error sending log:", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Println("❌ Error response from log API:", resp.Status)
		return fmt.Errorf("error response from log API: %s", resp.Status)
	}
	return nil
}
