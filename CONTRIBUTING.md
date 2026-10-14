# Contributing to Kamui Cloud

Thank you for helping build Kamui. This guide explains how we work. Please follow it so the project stays clean for everyone.

## Table of contents

- [Ground rules](#ground-rules)
- [Folders, owners and languages](#folders-owners-and-languages)
- [Workflow](#workflow)
- [Branch names](#branch-names)
- [Commit messages](#commit-messages)
- [Pull requests](#pull-requests)
- [Code standards](#code-standards)
- [What never goes in the repo](#what-never-goes-in-the-repo)
- [Getting help](#getting-help)

## Ground rules

1. Never push directly to `main`. Every change goes through a pull request.
2. Work inside your own folder. For changes in another folder, ask the owner or open an Issue.
3. Keep pull requests small and focused (one folder, one to two days of work).
4. Every task starts as an Issue.
5. Code and decisions live on GitHub. Use WhatsApp only for quick talk.

## Folders, owners and languages

Each folder has one owner, listed in [`.github/CODEOWNERS`](.github/CODEOWNERS). The owner reviews every pull request that touches the folder.

| Folder | Language | Purpose |
|---|---|---|
| `agent/` | Rust | Node agent: discovery, heartbeat |
| `api/` | Go | Coordinator: registry, scheduler, auth |
| `store/` | Go | S3-compatible storage |
| `run/` | Go | Container and job runner |
| `cli/` | Go | The `kamui` command |
| `train/` | Python | ML training |
| `infer/` | Python | Model serving |
| `console/` | TypeScript | Web dashboard |
| `drop/` | TypeScript | File sharing PWA |
| `shared/` | OpenAPI / Protobuf | API contracts |
| `ops/` | Shell, Docker | Setup and deployment |
| `docs/` | Markdown | Documentation |

## Workflow

```bash
# 1. Start from the latest main
git checkout main
git pull

# 2. Create a branch
git checkout -b agent/add-heartbeat

# 3. Do your work inside your folder, then commit
git add agent
git commit -m "agent: add heartbeat"

# 4. Push your branch
git push -u origin agent/add-heartbeat

# 5. Open a pull request
gh pr create --fill
```

Step by step:

1. **Pick or create an Issue.** Assign it to yourself and move it to **Doing** on the project board.
2. **Branch, code, commit, push** as shown above.
3. **Open a pull request.** Fill in the template. Move the Issue to **Review**.
4. **The folder owner reviews.** If changes are requested, fix them and push again. The pull request updates automatically.
5. **Merge.** After approval, the owner merges and deletes the branch. The Issue moves to **Done**.

## Branch names

Format: `<folder>/<short-task>`

Examples: `store/add-upload`, `agent/mdns-discovery`, `docs/fix-readme`

## Commit messages

Start with the folder name, then a short description in the present tense.

```
agent: add heartbeat
store: fix upload size check
docs: update setup steps
```

## Pull requests

- One pull request touches one folder.
- Link the Issue with `Closes #12` in the description.
- Describe how you tested the change.
- Keep it small. Large changes are hard to review.
- Do not merge your own pull request without approval.

## Code standards

Run these before you push.

**Rust** (`agent/`)
```bash
cargo fmt
cargo clippy -- -D warnings
cargo test
```

**Go** (`api/`, `store/`, `run/`, `cli/`)
```bash
gofmt -w .
go vet ./...
go test ./...
```

**Python** (`train/`, `infer/`)
```bash
uv run ruff format .
uv run ruff check .
uv run pytest
```

**TypeScript** (`console/`, `drop/`)
```bash
npm run lint
npm test
```

Every folder should contain a `README.md` that explains how to build and run it, and a `Dockerfile` where it makes sense.

## What never goes in the repo

The repository is public. Never commit:

- Secrets, tokens, passwords, `.env` files, private keys
- Datasets, model weights, checkpoints
- Build output (`target/`, `bin/`, `node_modules/`, `.venv/`)

If you commit a secret by mistake, tell the project head immediately so it can be rotated.

## Getting help

Stuck? Try for 30 minutes, then ask. Open an Issue or message the folder owner.

## Onboarding checklist

- [ ] Install Git, GitHub CLI, Rust, Go and Docker
- [ ] Accept the organization invite and turn on 2FA
- [ ] Clone the repository
- [ ] Add your name to `CONTRIBUTORS.md` through a pull request
