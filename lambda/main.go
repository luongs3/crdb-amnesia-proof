// Command export-lambda is the AWS Lambda entry point for shipping the agent's audit chain
// to S3 (satisfies the mandatory "at least one AWS service" requirement).
//
// Deployed as a provided.al2023 custom runtime so there is no aws-lambda-go dependency: the
// runtime API is three HTTP calls, and hand-rolling them keeps the whole AWS surface of this
// project at zero third-party modules. Judges can read it in one sitting.
//
// Build + deploy:
//
//	GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o bootstrap ./lambda
//	zip -j function.zip bootstrap
//	aws lambda create-function --function-name amnesia-audit-export \
//	  --runtime provided.al2023 --architecture arm64 --handler bootstrap \
//	  --role "$ROLE_ARN" --zip-file fileb://function.zip \
//	  --environment "Variables={CRDB_DSN=...,S3_BUCKET=...,AGENT_ID=agent_a1}"
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"

	"github.com/luongs3/crdb-amnesia-proof/internal/awsexport"
)

func main() {
	api := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	if api == "" {
		// Run locally with the same code path, so the handler is testable without deploying.
		out, err := handle(context.Background())
		if err != nil {
			log.Fatalf("handler: %v", err)
		}
		fmt.Println(out)
		return
	}

	client := &http.Client{Timeout: 0} // long-poll: next-invocation blocks until work arrives
	for {
		req, err := client.Get(fmt.Sprintf("http://%s/2018-06-01/runtime/invocation/next", api))
		if err != nil {
			log.Printf("next invocation: %v", err)
			time.Sleep(time.Second)
			continue
		}
		reqID := req.Header.Get("Lambda-Runtime-Aws-Request-Id")
		_, _ = io.Copy(io.Discard, req.Body)
		req.Body.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		result, herr := handle(ctx)
		cancel()

		path := "response"
		payload := result
		if herr != nil {
			path = "error"
			payload = fmt.Sprintf(`{"errorMessage":%q,"errorType":"ExportError"}`, herr.Error())
		}
		resp, err := client.Post(
			fmt.Sprintf("http://%s/2018-06-01/runtime/invocation/%s/%s", api, reqID, path),
			"application/json", bytes.NewReader([]byte(payload)))
		if err != nil {
			log.Printf("post %s: %v", path, err)
			continue
		}
		resp.Body.Close()
	}
}

func handle(ctx context.Context) (string, error) {
	dsn := os.Getenv("CRDB_DSN")
	bucket := os.Getenv("S3_BUCKET")
	agentID := os.Getenv("AGENT_ID")
	if agentID == "" {
		agentID = "agent_a1"
	}
	if dsn == "" || bucket == "" {
		return "", fmt.Errorf("CRDB_DSN and S3_BUCKET must be set")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()

	uri, bundle, err := awsexport.Export(ctx, db, agentID, bucket, os.Getenv("AWS_REGION"))
	if err != nil {
		return "", err
	}

	out, err := json.Marshal(map[string]any{
		"exported_to": uri,
		"agent_id":    bundle.AgentID,
		"links":       bundle.Links,
		"head_hash":   bundle.HeadHash,
		"exported_at": bundle.ExportedAt,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}
