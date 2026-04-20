// Command basic demonstrates the core fabricsdk API:
// connecting to a peer, reading a ledger record, and writing one.
//
// Configure via environment variables before running:
//
//	export PEER_ENDPOINT=localhost:7051
//	export PEER_HOST_OVERRIDE=peer0.org1.example.com
//	export TLS_CERT_PATH=/path/to/peer-tls-ca.pem
//	export CERT_PATH=/path/to/msp/signcerts
//	export KEY_PATH=/path/to/msp/keystore
//	export MSP_ID=Org1MSP
//	export CHANNEL=mychannel
//	export CHAINCODE=basic
//
//	go run main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	fabricsdk "github.com/muhammadtalha198/fabricsdk"
)

func main() {
	cfg := fabricsdk.Config{
		PeerEndpoint:     mustEnv("PEER_ENDPOINT"),
		PeerHostOverride: mustEnv("PEER_HOST_OVERRIDE"),
		TLSCertPath:      mustEnv("TLS_CERT_PATH"),
		CertPath:         mustEnv("CERT_PATH"),
		KeyPath:          mustEnv("KEY_PATH"),
		MSPID:            mustEnv("MSP_ID"),
		ChannelName:      mustEnv("CHANNEL"),
		ChaincodeName:    mustEnv("CHAINCODE"),
	}

	// ── Connect ──────────────────────────────────────────────────────────────
	fc, err := fabricsdk.New(cfg)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer fc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ── Read (Evaluate) ───────────────────────────────────────────────────────
	fmt.Println("→ Reading all assets...")
	raw, err := fc.Evaluate(ctx, "GetAllAssets")
	if err != nil {
		log.Fatalf("GetAllAssets: %v", err)
	}

	var assets []map[string]any
	if err := json.Unmarshal(raw, &assets); err != nil {
		log.Fatalf("parse: %v", err)
	}
	fmt.Printf("  found %d assets\n", len(assets))

	// ── Write (Submit) ────────────────────────────────────────────────────────
	fmt.Println("→ Creating asset asset-sdk-demo ...")
	asset := map[string]any{
		"ID":             "asset-sdk-demo",
		"Color":          "blue",
		"Size":           5,
		"Owner":          "fabricsdk-user",
		"AppraisedValue": 300,
	}
	payload, _ := json.Marshal(asset)

	txID, err := fc.Submit(ctx, "CreateAsset", string(payload))
	if err != nil {
		log.Fatalf("CreateAsset: %v", err)
	}
	fmt.Printf("  committed txID=%s\n", txID)

	// ── Error handling demo ───────────────────────────────────────────────────
	fmt.Println("→ Fetching non-existent asset (expect 404)...")
	_, err = fc.Evaluate(ctx, "ReadAsset", "does-not-exist")
	if fabricsdk.IsNotFound(err) {
		fmt.Println("  got expected 404 — IsNotFound=true ✓")
	} else if err != nil {
		fmt.Printf("  unexpected error: %v\n", err)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}
