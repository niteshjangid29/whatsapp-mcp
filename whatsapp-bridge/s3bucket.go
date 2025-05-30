package main

// func uploadToS3(bucketName string, key string, data []byte) (string, error) {
// 	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(os.Getenv("AWS_REGION")))
// 	if err != nil {
// 		return "", err
// 	}

// 	s3Client := s3.NewFromConfig(cfg)

// 	contentType := http.DetectContentType(data)

// 	_, err = s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
// 		Bucket:      aws.String(bucketName),
// 		Key:         aws.String(key),
// 		Body:        bytes.NewReader(data),
// 		ContentType: aws.String(contentType),
// 	})
// 	if err != nil {
// 		return "", err
// 	}

// 	url := "https://" + bucketName + ".s3." + os.Getenv("AWS_REGION") + ".amazonaws.com/" + key
// 	return url, nil
// }

// func getPreSignedURL(bucketName string, key string, expiry time.Duration) (string, error) {
// 	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(os.Getenv("AWS_REGION")))
// 	if err != nil {
// 		return "", err
// 	}
// 	s3Client := s3.NewFromConfig(cfg)
// 	presigner := s3.NewPresignClient(s3Client)

// 	presignResult, err := presigner.PresignGetObject(context.TODO(), &s3.GetObjectInput{
// 		Bucket: aws.String(bucketName),
// 		Key:    aws.String(key),
// 	}, s3.WithPresignExpires(expiry))
// 	if err != nil {
// 		return "", err
// 	}

// 	return presignResult.URL, nil
// }
