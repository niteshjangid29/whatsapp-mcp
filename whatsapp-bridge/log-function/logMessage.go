package logfunction

import (
	"bytes"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

const LogAPIEndpoint = "https://backend.railse.com/whatsapp/log-message"

func LogMessage(senderPhone string, text string, recipientPhone string, messageTime time.Time) error {
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

	// log.Println("📤 REQUEST", req)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Println("❌ Error sending log:", err)
		return err
	}
	defer resp.Body.Close()

	return nil
}
