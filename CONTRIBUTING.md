# Contributing to Kamui Cloud

A simple workflow, built so nobody gets conflicts.

## The idea

- Every person owns one folder. Work **only in your folder**.
- Nobody touches the same files, so conflicts do not happen.
- Everyone pushes straight to `main`. No pull requests.

## One-time setup

1. Accept the invite for the GitHub organization `nimbuslabs-dev` and turn on 2FA.
2. Install Git, GitHub CLI (`gh`) and VS Code. Rust, Go and Docker come later.
3. Log in:
   ```bash
   gh auth login
   gh auth setup-git
   ```
4. Download the project:
   ```bash
   git clone https://github.com/nimbuslabs-dev/kamui-cloud.git
   cd kamui-cloud
   ```

## Daily workflow

```bash
cd ~/kamui-cloud
git pull                              # 1. get the latest code
# 2. work inside your own folder only
git add agent                         # 3. add ONLY your folder
git commit -m "agent: add heartbeat"  # 4. save your work
git pull --rebase                     # 5. get any new changes
git push                              # 6. upload
```

Replace `agent` with your own folder name.

- Pull before you start.
- Commit small.
- Push at least once a day.

## Rules

**Do**
- Work only in your own folder.
- Start every commit message with your folder name: `store: add upload`.
- Use `git add <your-folder>`, not `git add .`.
- Ask the owner if you need a change in someone else's folder.

**Do not**
- Edit another person's folder.
- Edit the repo root files, `.github/`, `shared/` or `ops/` (head only, ask first).
- Use `git push --force`.
- Commit secrets, `.env` files, passwords, datasets or model files. The repo is public.

## Folders and languages

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
| `shared/` | OpenAPI | API contracts |
| `ops/` | Shell, Docker | Setup and deploy scripts |
| `docs/` | Markdown | Documentation |

## If something goes wrong

| Problem | Fix |
|---|---|
| `git push` says rejected or "fetch first" | `git pull --rebase`, then `git push` |
| Error 403 or permission denied | `gh auth login`, then `gh auth setup-git` |
| Message says `CONFLICT` | Do not panic and do not force push. Send a screenshot to the head. |
| `git status` shows files you did not change | Leave them. Add only your folder. |
| `not a git repository` | Wrong folder. Run `cd ~/kamui-cloud` |
| You committed a secret | Tell the head immediately. |

## Tasks

Tasks live on the project board: To do, Doing, Review, Done.

1. Take a task and move it to **Doing**. Keep only 1 or 2 there.
2. After you push, move it to **Review**.
3. The owner checks it and moves it to **Done**.

## First task

Create a `README.md` inside your own folder with one line about what it will do. Push it. This proves your setup works.
