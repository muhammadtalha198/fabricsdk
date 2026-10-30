# fabricsdk

[![Go Reference](https://pkg.go.dev/badge/github.com/muhammadtalha198/fabricsdk.svg)](https://pkg.go.dev/github.com/muhammadtalha198/fabricsdk)
[![CI](https://github.com/muhammadtalha198/fabricsdk/actions/workflows/ci.yml/badge.svg)](https://github.com/muhammadtalha198/fabricsdk/actions)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**A small, ethers.js-style Go client for Hyperledger Fabric.**

`fabricsdk` wraps the official [Fabric Gateway client](https://github.com/hyperledger/fabric-gateway)
(`github.com/hyperledger/fabric-gateway`) in a short API: `Evaluate` for reads, `Submit` for
writes. It handles TLS, gRPC, identities, endorsement, ordering and commit waiting, and adds
what production services need on top: context-aware timeouts, typed errors with retry
guidance, safe handling of "did my transaction commit?", private-data options, per-user
identities on one connection, and event listeners that resume from a checkpoint.

Requires Fabric **v2.4.4 or later** (tested against the Fabric v2.5 Gateway protocol) and
**Go 1.26+**.

---

## Install

```bash
go get github.com/muhammadtalha198/fabricsdk@v0.2.0
```

---

## Quickstart

```go
fc, err := fabricsdk.New(fabricsdk.Config{
    PeerEndpoint:     "localhost:7051",
    PeerHostOverride: "peer0.org1.example.com",
    TLSCertPath:      "/path/to/peer/tls/ca.crt",           // the peer's TLS CA
    CertPath:         "/path/to/User1@org1/msp/signcerts",   // file or directory
    KeyPath:          "/path/to/User1@org1/msp/keystore",    // file or directory
    MSPID:            "Org1MSP",
    ChannelName:      "mychannel",
    ChaincodeName:    "basic",
})
if err != nil {
    log.Fatal(err)
}
defer fc.Close()

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

// Read: runs on a peer, writes nothing.
raw, err := fc.Evaluate(ctx, "ReadAsset", "asset1")

// Write: endorse, order, wait for the commit.
res, err := fc.Submit(ctx, "CreateAsset", "asset2", "blue", "5", "Tomoko", "300")
fmt.Println(res.TxID, res.BlockNumber, string(res.Payload))
```

Runnable programs: [`examples/basic`](examples/basic) and [`examples/events`](examples/events).

---

## Timeouts and cancellation

Every call takes a `context.Context`. A deadline or cancellation on it bounds the whole call.
When the context has **no** deadline, each phase uses its own default:

| Phase | Config field | Default |
|---|---|---|
| Evaluate | `EvaluateTimeout` | 5s |
| Endorse | `EndorseTimeout` | 15s |
| Submit to orderer | `SubmitTimeout` | 5s |
| Wait for commit | `CommitStatusTimeout` | 60s |

---

## Private data and endorsement control

Send private or personal data as **transient** data. Transient data reaches the endorsing
peers but is never written into the block.

```go
res, err := fc.SubmitWith(ctx, "CreateChallan",
    fabricsdk.Args("CH-2026-000001"),
    fabricsdk.Transient(map[string][]byte{"owner": ownerJSON}),
)
```

`fabricsdk.EndorsingOrgs("Org1MSP")` restricts endorsement to named organizations. The
Gateway normally picks endorsers itself, so only set this when Fabric requires it: when the
Gateway's own organization cannot endorse a proposal that carries transient data, or for
blind writes to a private collection. `fabricsdk.BytesArgs` passes raw byte arguments.

---

## Identities: one connection, many users

Load an identity once and reuse it. Every handle shares the client's single gRPC connection,
so switching identity costs no network handshake.

```go
officer, err := fabricsdk.LoadIdentity("PoliceMSP", "/certs/officer-0001.pem", "/keys/officer-0001.pem")
res, err := fc.WithIdentity(officer).Submit(ctx, "IssueChallan", challanID)
```

- `LoadIdentity` accepts files, or MSP directories that hold exactly one file. If a
  directory holds several files, it returns an error instead of guessing.
- Every constructor checks that the private key matches the certificate.
- `NewIdentity(mspID, cert, sign)` accepts any signing function, for example a remote signing
  service.
- `IdentityFromHSM` keeps the private key inside a PKCS#11 HSM. It needs the build tag
  `pkcs11` and `CGO_ENABLED=1`:

```go
//go:build pkcs11
factory, err := identity.NewHSMSignerFactory("/usr/lib/softhsm/libsofthsm2.so")
defer factory.Dispose()
officer, err := fabricsdk.IdentityFromHSM(factory, "PoliceMSP", certPEM,
    fabricsdk.HSMOptions{Label: "ForFabric", Pin: pinFromSecretStore})
defer officer.Close()
```

The HSM library serialises signing for each signer, so give each officer their own identity
rather than sharing one.

---

## Other channels, chaincodes and contracts

```go
requests := fc.Target("consortium-channel", "requests", "RequestContract")
raw, err := requests.WithIdentity(officer).Evaluate(ctx, "GetRequest", id)
```

Handles are cheap values. You can create one for every request.

---

## Errors

Every call returns an `*fabricsdk.Error`. The helpers below see through `fmt.Errorf("%w")`
wrapping.

| Helper | Meaning | Typical HTTP code |
|---|---|---|
| `IsNotFound` | Record does not exist (from chaincode) | 404 |
| `IsAlreadyExists` | Record already exists. For idempotent writes, treat as done | 409 |
| `IsConflict` | MVCC or phantom read conflict at commit, or chaincode `CONFLICT`. Re-read and retry | 409 |
| `IsUnauthorized` | Chaincode `UNAUTHORIZED`, or Fabric denied access | 403 |
| `IsInvalidArgument` | Bad input | 400 |
| `IsTimeout` | Timed out before the transaction could reach the orderer | 504 |
| `IsOutcomeUnknown` | The transaction **may already be on the ledger** | 504 |
| `IsRetryable` | Sending the same request again is safe | — |
| `IsChaincode` | The error came from chaincode logic | — |

```go
res, err := fc.Submit(ctx, "IssueChallan", id)
var fe *fabricsdk.Error
switch {
case err == nil:
case fabricsdk.IsAlreadyExists(err):
    // Duplicate of an earlier, successful submit: treat as success.
case errors.As(err, &fe) && fe.OutcomeUnknown:
    // Never resubmit blindly. Resolve with the transaction ID.
    st, _ := fc.TxStatus(ctx, fe.TxID) // st.Found, st.Valid, st.ValidationCode
case fabricsdk.IsRetryable(err):
    // Safe to retry, with backoff.
default:
    http.Error(w, fe.Message, fe.Code)
}
```

Each `Error` carries:
- `Kind` and `Source`.
- An HTTP-style `Code`.
- The `TxID`, whenever one existed.
- The commit `ValidationCode`.
- The gRPC code.
- Per-peer `Details`.
- The underlying `Cause`, from the fabric-gateway typed errors.

### Chaincode error codes (`fabricsdk/ccerr`)

Fabric v2.5 sends chaincode errors to the client as plain text, with the same gRPC status the
Gateway uses for peer failures. To classify them reliably, return errors from the
dependency-free `ccerr` package in your chaincode:

```go
import "github.com/muhammadtalha198/fabricsdk/ccerr"

return ccerr.New(ccerr.NotFound, "challan %s does not exist", id) // "NOT_FOUND: challan ... does not exist"
```

Codes: `INVALID_ARGUMENT`, `NOT_FOUND`, `ALREADY_EXISTS`, `CONFLICT`, `UNAUTHORIZED`,
`FAILED_PRECONDITION`.

For chaincode that does not use `ccerr` yet, the SDK falls back to keywords such as
`not found`, `does not exist`, `already exists`, `version conflict` and `unauthorized`. It
skips known peer-side wording, so "chaincode basic not found" is not reported as a missing
record. Set `Config.StrictErrorCodes = true` to turn the fallback off.

---

## Transaction status and channel height

```go
st, err := fc.TxStatus(ctx, txID) // via qscc GetTransactionByID
h, err  := fc.Height(ctx)         // via qscc GetChainInfo; useful for health checks
```

`TxStatus` answers for the Gateway peer. `Found == false` means one of two things: the
transaction was never ordered, or that peer has not committed its block yet. The calling
identity needs the `qscc/GetTransactionByID` ACL, which is channel Readers by default.

For outbox-style processing, `SubmitAsync` returns after the orderer accepts the transaction.
Store `pending.TxID` durably, then call `pending.Wait(ctx)`.

---

## Events

### Resilient listeners (recommended)

```go
cp, _ := fabricsdk.NewFileCheckpointer("checkpoint.json")
defer cp.Close()

err := fc.ListenChaincodeEvents(ctx, cp, func(ctx context.Context, ev fabricsdk.ChaincodeEvent) error {
    return project(ctx, ev) // returning an error stops the listener; the event is not checkpointed
}, fabricsdk.FromBlock(0))
```

The listener works like this:
- It checkpoints after each handled event.
- When the peer drops the stream, it reconnects with backoff, starting from the checkpoint.
  No events are skipped.
- It returns `nil` when `ctx` ends.

A crash between your handler and the checkpoint can deliver an event again, so make handlers
idempotent. For exactly-once processing, implement `Checkpointer` on your database and save
the position in the same transaction as your data.

`ListenBlocks` does the same for full blocks, for building off-chain read models. Blocks
include invalid transactions; check the `TRANSACTIONS_FILTER` metadata. Private data values
are not included.

### Single stream

```go
s, err := fc.Events(ctx, fabricsdk.FromBlock(42))
for ev := range s.Events() { ... }
if err := s.Err(); err != nil { /* the peer ended the stream; reconnect */ }
```

Without `FromBlock` or a checkpoint, a stream starts at the next committed block.

---

## Logging and metrics

```go
cfg.Logger = fabricsdk.NewSlogLogger(slog.Default())
cfg.Observer = fabricsdk.ObserverFunc(func(ctx context.Context, c fabricsdk.CallInfo) {
    callDuration.WithLabelValues(c.Operation, c.Function, string(fabricsdk.KindOf(c.Err))).Observe(c.Duration.Seconds())
})
```

The SDK logs function names, transaction IDs and error kinds only. It never logs arguments or
transient data, because those may contain personal data. The `Observer` receives one
`CallInfo` per phase: `evaluate`, `endorse`, `submit`, `commit` and `tx_status`.

---

## Config reference

| Field | Required | Description |
|---|---|---|
| `PeerEndpoint` | ✅ | Gateway peer `host:port`. A bare `host:port` is dialled with `dns:///`. |
| `PeerHostOverride` | | TLS server name to verify. Defaults to the host in `PeerEndpoint`. |
| `TLSCertPath` | ✅ | PEM file with the peer's TLS CA certificate(s). |
| `ClientTLSCertPath`, `ClientTLSKeyPath` | | Mutual TLS client key pair. Set both or neither. |
| `KeepAlive`, `DialOptions` | | Extra gRPC settings. |
| `Identity` | | Default identity. If nil, the SDK loads one from `MSPID`, `CertPath` and `KeyPath`. |
| `MSPID`, `CertPath`, `KeyPath` | ✅ if no `Identity` | |
| `ChannelName`, `ChaincodeName` | ✅ | Default target. |
| `ContractName` | | Named contract. Empty means the default contract. |
| `EvaluateTimeout`, `EndorseTimeout`, `SubmitTimeout`, `CommitStatusTimeout` | | Per-phase defaults. See the timeout table above. |
| `StrictErrorCodes` | | Turns off the keyword fallback for chaincode errors. |
| `Logger`, `Observer` | | Optional hooks. |

`NewWithConnection(conn, cfg)` reuses an existing gRPC connection, which is useful in tests.

---

## Testing

The unit tests run the real fabric-gateway client against an in-process fake Gateway and
Deliver server, over an in-memory gRPC connection. No Fabric network is needed:

```bash
go test -race ./...
CGO_ENABLED=1 go vet -tags pkcs11 ./...
```

---

## Upgrading from v0.1

v0.2.0 is a breaking release. See [CHANGELOG.md](CHANGELOG.md) for the migration table.

## License

MIT. See [LICENSE](LICENSE).
