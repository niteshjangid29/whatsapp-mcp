package logfunction

import (
	"bytes"
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

func LogImageMessage(senderPhone string, text string, recipientPhone string, filePath string, messageTime time.Time) error {
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

	// log.Println("📤 Image REQUEST", req)

	clientHTTP := &http.Client{}
	apiResp, err := clientHTTP.Do(req)
	if err != nil {
		log.Println("❌ Error sending log:", err)
		return err
	}
	defer apiResp.Body.Close()

	log.Println("✅ Image message logged successfully")
	return nil
}
