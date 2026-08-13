// Command replaywebhook re-sends a synthetic Concept2 "result-added" webhook
// delivery, byte-for-byte in the same shape a real Concept2 delivery uses
// (see internal/concept2.Concept2Payload), to a running RowBot instance —
// for manually re-triggering processing of a result that failed the first
// time (e.g. after fixing a bug, or if the original delivery was lost).
//
// Both --result-id and --user-id are required: the webhook body itself is
// never trusted as a data source (see RowingService.ProcessResult) — it's
// only used to know WHAT result to re-fetch and WHOSE stored Concept2 OAuth
// token to fetch it with, and Concept2's API has no "look up a result by ID
// alone" endpoint. Both values are logged together on every real delivery
// ("concept2 webhook received" concept2_user_id=... result_id=... in server
// logs), so they're easy to find together when diagnosing a failed one.
//
// Usage:
//
//	go run ./cmd/replaywebhook --result-id 119434929 --user-id 1890109
//	go run ./cmd/replaywebhook --result-id 119434929 --user-id 1890109 --rowbot-endpoint http://localhost:8080
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	resultID := flag.Int64("result-id", 0, "Concept2 result ID to replay (required)")
	userID := flag.Int64("user-id", 0, "Concept2 user ID that owns the result (required) — see concept2_user_id in the original webhook's server log line")
	rowbotEndpoint := flag.String("rowbot-endpoint", "https://row-bot.xyz", "base URL of the RowBot instance to send the webhook to (e.g. http://localhost:8080 for a local instance) — /webhooks/concept2 is appended automatically")
	flag.Parse()

	if *resultID == 0 || *userID == 0 {
		fmt.Fprintln(os.Stderr, "usage: replaywebhook --result-id <id> --user-id <id> [--rowbot-endpoint <url>]")
		flag.PrintDefaults()
		os.Exit(2)
	}

	target := strings.TrimRight(*rowbotEndpoint, "/") + "/webhooks/concept2"

	// Matches internal/concept2.Concept2Payload's confirmed real shape
	// exactly: flat, no "data" wrapper, only type/result.id/result.user_id
	// are ever read by the handler.
	payload, err := json.Marshal(map[string]any{
		"type": "result-added",
		"result": map[string]any{
			"id":      *resultID,
			"user_id": *userID,
		},
	})
	if err != nil {
		log.Fatalf("marshal payload: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		log.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	fmt.Printf("POST %s\n%s\n", target, payload)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		log.Fatalf("send webhook: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	fmt.Printf("-> %s\n", resp.Status)
	if len(body) > 0 {
		fmt.Printf("%s\n", body)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		os.Exit(1)
	}
}
