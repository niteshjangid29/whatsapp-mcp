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

func LogImageMessageSQS(senderPhone string, text string, recipientPhone string, filePath string, messageTime time.Time) error {
	err := godotenv.Load()
	if err != nil {
		return err
	}
	bearerToken := os.Getenv("BEARER_TOKEN")
	if bearerToken == "" {
		return fmt.Errorf("BEARER_TOKEN not set in .env file")
	}

	resp, err := http.Get(filePath)
	if err != nil {
		log.Println("❌ Error fetching S3 file:", err)
		return err
	}
	defer resp.Body.Close()

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

	_, err = io.Copy(part, resp.Body)
	if err != nil {
		log.Println("❌ Error copying file data:", err)
		return err
	}

	writer.Close()

	req, err := http.NewRequest("POST", LogAPIEndpoint, body)
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
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

	if apiResp.StatusCode != http.StatusOK {
		log.Println("❌ Error response from log API:", apiResp.Status)
		return fmt.Errorf("error response from log API: %s", apiResp.Status)
	}

	// log.Println("✅ Image message logged successfully")
	return nil
}
