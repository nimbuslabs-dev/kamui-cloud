# Kamui Cloud — Setup Guide

## 1. Overview

This guide explains how contributors can prepare their development environment and start working on Kamui Cloud.

Kamui Cloud is designed as an intranet-first platform. Its goal is to enable authorized computers on a private network to share computing and other cloud-like services.

## 2. Prerequisites

Install the tools required for your assigned service.

| Tool | Purpose |
|---|---|
| Git | Source control and version management |
| GitHub CLI (`gh`) | GitHub authentication and repository operations |
| Visual Studio Code | Code editor |
| Python 3.12 | Python-based services |
| uv | Python environment and dependency management |
| Node.js LTS | TypeScript and JavaScript services |
| Docker | Container-based development and execution |
| Go | API, storage, execution, and CLI services |
| Rust | Node agent development |

Follow the repository's current instructions for exact versions and installation options. Not every contributor needs every language runtime.

## 3. Authenticate with GitHub

Open a terminal and run:

```bash
gh auth login
gh auth setup-git
gh auth status
```

Follow the authentication prompts. Confirm that your GitHub account has the permissions required for the repository.

## 4. Clone the Repository

Clone the repository:

```bash
git clone https://github.com/nimbuslabs-dev/kamui-cloud.git
cd kamui-cloud
```

Open it in VS Code:

```bash
code .
```

If the repository is already cloned, open the existing directory instead of cloning it again.

## 5. Check Your Environment

Run the checks relevant to your assigned service:

```bash
git --version
gh --version
python --version
uv --version
node --version
docker --version
go version
rustc --version
```

A missing command does not necessarily mean the entire project is broken. Install only the tools required for your task.

## 6. Understand the Repository

Before editing, inspect:

- `README.md`
- `CONTRIBUTING.md`
- Your assigned service folder
- Relevant documentation under `docs/`
- Existing tests and configuration files

Check the repository status:

```bash
git status
```

Do not overwrite existing changes you did not create.

## 7. Follow Folder Ownership

Each contributor is assigned a specific folder.

Rudra (`Rudra-1612`) owns `docs/`.

Root files, `.github/`, `shared/`, and `ops/` are head-only. Do not modify these areas unless explicitly authorized.

If your task requires a change in another contributor's folder, contact the owner or open a GitHub Issue.

## 8. Daily Git Workflow

Before starting work:

```bash
git status
git pull
```

Make changes only in your assigned folder.

Review your changes:

```bash
git diff -- docs/
```

Stage only your intended files:

```bash
git add docs/README.md
```

Replace the filename with the files you actually changed.

Review and commit:

```bash
git diff --cached
git commit -m "docs: update documentation"
```

Synchronize before pushing:

```bash
git pull --rebase
git push
```

If a conflict or permission error occurs, stop and resolve it with the relevant owner. Never force-push to bypass a rejection.

## 9. Troubleshooting

### GitHub authentication fails

Run `gh auth status` and complete `gh auth login` if necessary.

### Push is rejected

Fetch or pull the latest changes, inspect conflicts, and follow the repository's contribution rules. Check branch permissions if the push is still rejected.

### A command is not recognized

Verify that the tool is installed and available in your terminal's PATH. Restart the terminal after installation if necessary.

### A service does not start

Read the error message, inspect its dependency configuration, and check whether the required runtime and environment variables are available.

### Computers cannot communicate over the LAN

Check their network connection, configured coordinator address, firewall rules, and router client-isolation settings.

## 10. Security

- Never commit credentials or secrets.
- Do not expose administrative services unnecessarily.
- Use only authorized development machines and resources.
- Follow the project's authentication and authorization requirements.

## 11. Getting Help

If setup fails, share the exact command, error output, operating system, and affected service with the team. Do not include passwords, tokens, or other sensitive information.