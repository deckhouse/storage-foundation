# Hack Scripts

This folder contains auxiliary scripts for development and project maintenance.

## fuzzing/

Fuzzing harness for the `data-exporter` HTTP handlers: `make -C hack/fuzzing all`.
See [fuzzing/README.md](fuzzing/README.md) for the runbook.

## git_commits_after_tag.py

Script for getting a list of commits that are not included in the latest tag.

### Description

The script performs the following actions:

1. Switches to the main branch
2. Executes git fetch to get updates
3. Finds the latest tag in the repository
4. Displays a list of commits after the latest tag

### Usage

```bash
# From project root folder
python3 hack/git_commits_after_tag.py
```

### Requirements

- Python 3.6+
- Git repository

### Functionality

- **Automatic switch to main**: The script automatically switches to the main branch
- **Getting updates**: Executes `git fetch --all` to get the latest changes
- **Finding latest tag**: Finds the newest tag in the repository
- **List of commits**: Shows all commits after the latest tag
- **Detailed information**: Displays change statistics for each commit
- **Error handling**: Informative error messages

### Example Output

```
🚀 Script for getting commits after latest tag
============================================================
📍 Current branch: main
🔄 Switching to main branch...
✅ Successfully switched to main branch
🔄 Getting updates (git fetch)...
✅ Updates received successfully
🔍 Searching for latest tag...
✅ Latest tag: v0.2.4
🔍 Searching for commits after tag v0.2.4...
✅ Found 2 commits after tag v0.2.4

📋 List of commits after latest tag (2 commits):
============================================================
 1. a1b2c3d - feat: add new feature
 2. e4f5g6h - fix: resolve bug in authentication
============================================================
```

### Features

- Works with any git repositories
- Automatically handles absence of tags
- Shows detailed information about the first 10 commits
- Excludes merge commits from the list
- Supports colored output with emojis for better readability
