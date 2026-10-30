# Changelog

All notable changes to this project are documented here. The project follows
[Semantic Versioning](https://semver.org/). Before v1.0.0, minor versions may
contain breaking changes.

## v0.2.0

This release makes the SDK safe for production write paths. It is a **breaking release**.

### Fixed

- **Contexts are honoured.** In v0.1, `Evaluate` and `Submit` ignored their `ctx`
  argument. Only the static Config timeouts applied. Now:
  - Every phase (evaluate, endorse, submit, commit wait) uses the caller's deadline and
    cancellation.
  - When the context has no deadline, the Config timeouts still apply per phase.
- **Read conflicts can be retried.** A transaction invalidated at commit by
  `MVCC_READ_CONFLICT` or `PHANTOM_READ_CONFLICT` used to return a generic 500 error. It now
  returns `KindConflict`, so `IsConflict` and `IsRetryable` are true and `ValidationCode` is
  set.
- **Uncertain outcomes are reported.** When the commit status, or the broadcast to the
  orderer, cannot be confirmed, the error has `OutcomeUnknown = true` and carries the
  transaction ID. Previously the ID was lost, so callers retried and created duplicates.
- **Error predicates see through wrapping.** `IsNotFound`, `IsConflict`, `IsUnauthorized` and
  `IsChaincode` now use `errors.As`, so they work after `fmt.Errorf("...: %w", err)`.
- **Chaincode errors keep their context.**
  - They keep the underlying cause, the gRPC code, every per-peer detail and the transaction
    ID.
  - Peer-side messages such as "chaincode basic not found" are no longer misreported as
    `NotFound`.
  - "does not exist", the wording used by fabric-samples, now maps to `NotFound`.
- **Event streams fail visibly.**
  - A stream that the peer closes is now reported through `EventStream.Err`.
  - The forwarding goroutine no longer leaks when the consumer stops reading.
  - `FromBlock(0)` really starts at block 0. In v0.1, 0 meant "new events only".
- **Identity loading is strict.**
  - Loading from a directory now rejects directories with several files instead of picking
    one at random.
  - Hidden files such as `.DS_Store` are ignored.
  - Every constructor checks that the private key matches the certificate.
- `Close` is idempotent and reports errors. Calls made after `Close` fail with `KindConfig`.

### Added

- `SubmitWith` / `EvaluateWith` with call options:
  - `Args`
  - `BytesArgs`
  - `Transient`, for private data
  - `EndorsingOrgs`
- `SubmitAsync` returning a `PendingTx`. Its `TxID` can be stored before calling
  `Wait(ctx)`; `Wait` can be called again after a timeout.
- `TxResult` with `TxID`, `BlockNumber` and `Payload`, which is the transaction function's
  return value.
- `TxStatus(ctx, txID)` using qscc `GetTransactionByID`, and `Height(ctx)` using qscc
  `GetChainInfo`, for reconciliation and health checks.
- Identities as values: `Identity`, `LoadIdentity`, `IdentityFromPEM`, `NewIdentity` (custom
  signer), and `IdentityFromHSM` (build tag `pkcs11`). Use `fc.WithIdentity(id)` to sign as
  another user on the shared connection.
- `fc.Target(channel, chaincode, contract)` and the `Contract` handle, which replace
  `Dynamic()`.
- A typed error model:
  - `Kind`, `Source`, HTTP-style `Code`, `TxID`, `ValidationCode`, `GRPCCode`, `Details`,
    `OutcomeUnknown` and `Retryable()`.
  - New helpers: `IsAlreadyExists`, `IsInvalidArgument`, `IsTimeout`, `IsOutcomeUnknown`,
    `IsRetryable`, `KindOf` and `TxIDOf`.
- Package `ccerr`, so chaincode can return coded errors (`NOT_FOUND: ...`) that clients can
  classify reliably. It imports only the standard library.
- `Config.StrictErrorCodes`, which turns off the keyword fallback.
- Resilient listeners:
  - `ListenChaincodeEvents` and `ListenBlocks`, which checkpoint progress and reconnect with
    backoff.
  - The `Checkpointer` interface, with `NewMemoryCheckpointer` and `NewFileCheckpointer`
    implementations.
  - Event options: `FromBlock`, `WithCheckpointer`, `ForChaincode` and `RetryBackoff`.
- Mutual TLS: `ClientTLSCertPath` and `ClientTLSKeyPath`. The certificate hash is passed to
  the Gateway, as Fabric requires.
- `KeepAlive` and `DialOptions` settings, and `NewWithConnection` for shared connections and
  tests.
- An `Observer` hook for metrics, emitting one `CallInfo` per phase, and
  `NewSlogLogger`.
- Unit tests that run the real fabric-gateway client against an in-process fake Gateway and
  Deliver server. CI now runs the race detector, vets the `pkcs11` build and runs
  govulncheck.

### Changed

- Dependencies:

  | Module | v0.1 | v0.2 |
  |---|---|---|
  | `fabric-gateway` | v1.7.0 | v1.13.0 |
  | `fabric-protos-go-apiv2` | v0.3.4 | v0.3.7 |
  | `grpc` | v1.67.1 | v1.84.0 |
  | `protobuf` | v1.35.1 | v1.36.12 |
  | `x/net` | v0.28.0 | v0.62.0 |

- Minimum Go version raised to 1.26. This is still within the Go 1.26.x toolchain that
  ships in Fabric v2.5.16 chaincode images.
- The gRPC client is created with `grpc.NewClient`; `grpc.Dial` is deprecated. A bare
  `host:port` endpoint is dialled with the `dns:///` resolver.
- TLS: the CA file may hold several certificates, and TLS 1.2 is the minimum.

### Removed

- `Dynamic()`, `DynamicClient` and `DynamicCallerClient`. Use `fc.Target(...)`.
- `WithIdentity(certPath, keyPath)` on the client. Load the identity once with
  `LoadIdentity` and pass it to `fc.WithIdentity(id)`.
- `EventsFrom(ctx, block)`. Use `Events(ctx, FromBlock(block))`.
- `Error.Source` values `ErrorSourceSDK` and `ErrorSourceChaincode`. Use `SourceSDK`,
  `SourceChaincode` and `SourceNetwork`.

### Migration from v0.1

| v0.1 | v0.2 |
|---|---|
| `txID, err := fc.Submit(ctx, fn, args...)` | `res, err := fc.Submit(ctx, fn, args...)`, then `res.TxID` |
| `fc.WithIdentity(certDir, keyDir).Submit(...)` | `id, err := fabricsdk.LoadIdentity(msp, certDir, keyDir)` once, then `fc.WithIdentity(id).Submit(...)` |
| `fc.Dynamic().Evaluate(ctx, ch, cc, ct, fn, args...)` | `fc.Target(ch, cc, ct).Evaluate(ctx, fn, args...)` |
| `events, err := fc.Events(ctx)`, then `for ev := range events` | `s, err := fc.Events(ctx)`, then `for ev := range s.Events()`, and check `s.Err()` |
| `fc.EventsFrom(ctx, n)` | `fc.Events(ctx, fabricsdk.FromBlock(n))` |
| `IsConflict(err)` for "already exists" | `IsAlreadyExists(err)`. `IsConflict` now means "re-read and retry" |
| `fabErr.Source == fabricsdk.ErrorSourceChaincode` | `fabricsdk.IsChaincode(err)` |
| Creating a client per HTTP request | Create one client at startup and share it, using `WithIdentity` per user |

## v0.1.0

- Initial release.
