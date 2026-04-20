# fabricsdk
Lightweight Hyperledger Fabric SDK for Go with context support, events, and logging.

[![Go Reference](https://pkg.go.dev/badge/github.com/muhammadtalha198/fabricsdk.svg)](https://pkg.go.dev/github.com/muhammadtalha198/fabricsdk)
[![CI](https://github.com/muhammadtalha198/fabricsdk/actions/workflows/ci.yml/badge.svg)](https://github.com/muhammadtalha198/fabricsdk/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/muhammadtalha198/fabricsdk)](https://goreportcard.com/report/github.com/muhammadtalha198/fabricsdk)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**A lightweight, ethers.js-style Go SDK for Hyperledger Fabric.**

The official Fabric SDK is powerful but verbose. This SDK wraps the Fabric Gateway client into a clean, minimal API that feels like [ethers.js](https://docs.ethers.org) for Ethereum — you call `Evaluate` for reads and `Submit` for writes, and the SDK handles TLS, gRPC, identity, endorsement, and commit waiting for you.

---

## Install

```bash
go get github.com/muhammadtalha198/fabricsdk@v0.1.0
```

Requires Go 1.21+.

---

## Quickstart

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "time"

    fabricsdk "github.com/muhammadtalha198/fabricsdk"
)

func main() {
    cfg := fabricsdk.Config{
        PeerEndpoint:     "localhost:7051",
        PeerHostOverride: "peer0.org1.example.com",
        TLSCertPath:      "/path/to/peer-tls-ca.pem",
        CertPath:         "/path/to/msp/signcerts",
        KeyPath:          "/path/to/msp/keystore",
        MSPID:            "Org1MSP",
        ChannelName:      "mychannel",
        ChaincodeName:    "mychaincode",
    }

    fc, err := fabricsdk.New(cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer fc.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // Read (no ledger write)
    raw, err := fc.Evaluate(ctx, "GetAsset", "asset-1")
    if err != nil {
        log.Fatal(err)
    }

    var asset map[string]any
    json.Unmarshal(raw, &asset)
    fmt.Println(asset)

    // Write (endorse → submit → wait for commit)
    txID, err := fc.Submit(ctx, "CreateAsset", `{"ID":"asset-2","Color":"red","Size":5}`)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("committed:", txID)
}
```

---

## API

### Connect

```go
fc, err := fabricsdk.New(cfg)
defer fc.Close()
```

One persistent gRPC connection. Thread-safe — share across goroutines.

---

### Read

```go
raw, err := fc.Evaluate(ctx, "GetAsset", "asset-1")
```

Queries one peer. Never modifies ledger state.

---

### Write

```go
txID, err := fc.Submit(ctx, "CreateAsset", string(jsonBytes))
```

Endorses the proposal, submits to orderer, waits for commit, then returns the transaction ID. Equivalent to ethers.js `tx.wait()`.

---

### Per-call identity override

```go
txID, err := fc.WithIdentity("/admin/signcerts", "/admin/keystore").
    Submit(ctx, "DeleteAsset", "asset-1")
```

Reuses the existing gRPC connection — no extra handshake.

---

### Chaincode events

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

events, err := fc.Events(ctx)
if err != nil { ... }

for ev := range events {
    fmt.Printf("event=%s txID=%s payload=%s\n", ev.EventName, ev.TxID, ev.Payload)
}
```

Replay from a known block (useful after a restart):

```go
events, err := fc.EventsFrom(ctx, 42)
```

---

### Dynamic calls (multiple chaincodes)

```go
dyn := fc.Dynamic()

raw, err  := dyn.Evaluate(ctx, "channelA", "chaincodeA", "ContractA", "GetAsset", "id-1")
txID, err := dyn.Submit(ctx, "channelB", "chaincodeB", "ContractB", "CreateAsset", payload)
```

> **Note:** `DynamicClient` opens a new Gateway per call intentionally — it
> prioritises flexibility over connection reuse. For high-throughput paths, use
> `fc.Evaluate` / `fc.Submit` which reuse one Gateway.

---

### Structured errors

Every method returns a `*fabricsdk.Error` you can inspect without string parsing:

```go
txID, err := fc.Submit(ctx, "CreateAsset", string(jsonBytes))
if fabricsdk.IsNotFound(err)    { /* 404 */ }
if fabricsdk.IsConflict(err)    { /* 409 — retry */ }
if fabricsdk.IsUnauthorized(err){ /* 403 */ }

var fabErr *fabricsdk.Error
if errors.As(err, &fabErr) {
    http.Error(w, fabErr.Message, fabErr.Code)
}
```

| Predicate | Code | Meaning |
|---|---|---|
| `IsNotFound` | 404 | Asset / key does not exist |
| `IsConflict` | 409 | Optimistic concurrency conflict or duplicate key |
| `IsUnauthorized` | 403 | Chaincode rejected the caller |
| `IsChaincode` | — | Error came from inside chaincode logic |

---

### Optional logging

Plug in your own logger (zap, logrus, slog, etc.):

```go
type zapAdapter struct{ l *zap.SugaredLogger }
func (a *zapAdapter) Info(msg string, kv ...any)  { a.l.Infow(msg, kv...) }
func (a *zapAdapter) Error(msg string, kv ...any) { a.l.Errorw(msg, kv...) }

cfg.Logger = &zapAdapter{l: logger.Sugar()}
```

If `Logger` is nil, all log output is suppressed.

---

## Config reference

| Field | Required | Default | Description |
|---|---|---|---|
| `PeerEndpoint` | ✅ | — | `host:port` of the peer gRPC endpoint |
| `PeerHostOverride` | ✅ | — | TLS SNI hostname (peer cert CN/SAN) |
| `TLSCertPath` | ✅ | — | Path to peer TLS CA cert PEM file |
| `CertPath` | ✅ | — | Directory with signing cert (MSP signcerts) |
| `KeyPath` | ✅ | — | Directory with private key (MSP keystore) |
| `MSPID` | ✅ | — | MSP ID, e.g. `"Org1MSP"` |
| `ChannelName` | ✅ | — | Fabric channel name |
| `ChaincodeName` | ✅ | — | Chaincode name |
| `ContractName` | ❌ | `""` | Named contract; empty = default |
| `EvaluateTimeout` | ❌ | `5s` | Timeout for Evaluate calls |
| `EndorseTimeout` | ❌ | `15s` | Timeout for endorsement |
| `SubmitTimeout` | ❌ | `5s` | Timeout for orderer submit |
| `CommitStatusTimeout` | ❌ | `60s` | Timeout for commit wait |
| `Logger` | ❌ | `nil` | Structured logger (see above) |

---

## Examples

Copy-paste runnable examples are in [`/examples`](./examples):

- [`examples/basic`](./examples/basic) — connect, evaluate, submit, error handling
- [`examples/events`](./examples/events) — real-time chaincode event streaming

---

## Compared to the official SDK

| | fabricsdk | fabric-gateway (official) |
|---|---|---|
| Lines of boilerplate to connect | ~10 | ~60–100 |
| context.Context support | ✅ | ✅ |
| Chaincode events | ✅ | ✅ |
| Connection profile YAML | ❌ (planned) | ✅ |
| Typed contract generators | ❌ (planned) | ❌ |
| Structured errors | ✅ | ❌ |
| Named error predicates | ✅ | ❌ |

---

## Roadmap

- [ ] Connection profile YAML support
- [ ] Typed contract wrapper generator (like ABI in ethers.js)
- [ ] Prometheus metrics middleware
- [ ] Integration test suite (docker-compose test network)

---

## License

MIT — see [LICENSE](LICENSE).
