# How to Publish fabricsdk as an Official Go Package

This is your complete, step-by-step guide from zero to a public,
installable Go module on pkg.go.dev.

---

## PART 1 — Create the GitHub Repository

### Step 1 — Create the repo on GitHub

1. Go to https://github.com/new
2. Fill in:
   - **Repository name:** `fabricsdk`
   - **Description:** `Lightweight ethers.js-style Go SDK for Hyperledger Fabric`
   - **Visibility:** ✅ Public (required for pkg.go.dev)
   - **Do NOT** tick "Add a README" (you already have one)
3. Click **Create repository**

---

### Step 2 — Push your code

Open a terminal in your fabricsdk folder and run these commands one by one:

```bash
# 1. Initialize git
git init

# 2. Stage everything
git add .

# 3. First commit
git commit -m "feat: initial fabricsdk v0.1.0"

# 4. Rename branch to main (GitHub default)
git branch -M main

# 5. Connect to your GitHub repo
git remote add origin https://github.com/muhammadtalha198/fabricsdk.git

# 6. Push
git push -u origin main
```

After this, refresh your GitHub page — you should see all your files there.

---

## PART 2 — Tag a Release (This makes it a real versioned package)

Go modules require a git tag to be a proper versioned package.
Without this, nobody can install a specific version.

```bash
# Create the tag locally
git tag v0.1.0

# Push the tag to GitHub
git push origin v0.1.0
```

Now go to https://github.com/muhammadtalha198/fabricsdk/releases and you
will see "v0.1.0" listed there. Click it and press "Create release" to
add release notes if you want.

---

## PART 3 — Publish to pkg.go.dev (The official Go package index)

pkg.go.dev automatically indexes public Go modules. You just have to
trigger it once.

### Option A — Automatic (recommended)

Run this command anywhere on your machine that has Go installed:

```bash
GOPROXY=proxy.golang.org go list -m github.com/muhammadtalha198/fabricsdk@v0.1.0
```

This tells the Go proxy to fetch and cache your module. Within a few
minutes it will appear at:

```
https://pkg.go.dev/github.com/muhammadtalha198/fabricsdk
```

### Option B — Manual trigger

Go to https://pkg.go.dev/github.com/muhammadtalha198/fabricsdk and click
the "Request" button if it appears.

---

## PART 4 — Verify the installation works

On any machine with Go installed (or your own machine in a fresh folder):

```bash
mkdir test-fabricsdk && cd test-fabricsdk
go mod init test

go get github.com/muhammadtalha198/fabricsdk@v0.1.0
```

You should see:

```
go: added github.com/muhammadtalha198/fabricsdk v0.1.0
```

That's it. Your SDK is now an official, installable Go package.

---

## PART 5 — Add the Badges to your README

The badges in your README.md already point to the right URLs.
They will become active once:

| Badge | When it activates |
|---|---|
| Go Reference (pkg.go.dev) | After PART 3 above |
| CI (GitHub Actions) | After your first push (PART 2) |
| Go Report Card | Visit https://goreportcard.com/report/github.com/muhammadtalha198/fabricsdk once |

---

## PART 6 — How to release future versions

Every time you make changes and want to release a new version:

```bash
# Make your code changes, then:
git add .
git commit -m "feat: add connection profile YAML support"

git tag v0.2.0
git push origin main
git push origin v0.2.0
```

Follow semantic versioning (semver):
- `v0.1.0` → `v0.1.1`  for bug fixes
- `v0.1.0` → `v0.2.0`  for new features (backward compatible)
- `v1.0.0` → `v2.0.0`  for breaking API changes (also requires changing go.mod to `module .../v2`)

---

## PART 7 — Making it feel even more official (optional but recommended)

### Add a Go Report Card

1. Visit https://goreportcard.com/report/github.com/muhammadtalha198/fabricsdk
2. Click "Generate Report"
3. It will give you an A or A+ — the badge in your README links to this automatically

### Enable GitHub Discussions

In your GitHub repo → Settings → General → Features → tick "Discussions"

This gives users a place to ask questions (like a forum for your SDK).

### Add Topics on GitHub

In your repo page, click the gear icon next to "About" and add topics:
`hyperledger-fabric`, `blockchain`, `go`, `sdk`, `fabric-gateway`

This makes your repo discoverable in GitHub search.

---

## Summary — The 4 commands that matter

```bash
# 1. Push code
git push -u origin main

# 2. Tag release
git tag v0.1.0 && git push origin v0.1.0

# 3. Trigger pkg.go.dev indexing
GOPROXY=proxy.golang.org go list -m github.com/muhammadtalha198/fabricsdk@v0.1.0

# 4. Verify install
go get github.com/muhammadtalha198/fabricsdk@v0.1.0
```

After these 4 steps, your SDK is live.
