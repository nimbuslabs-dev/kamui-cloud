# Kamui Cloud — Daily Progress Log

## 1. Purpose

This document provides a consistent format for tracking daily development work, completed tasks, test results, blockers, and next steps.

Every contributor should record actual progress rather than planned or assumed achievements.

## 2. Daily Progress Template

Copy this template for each working day.

### Date: YYYY-MM-DD

**Contributor:**  
**Assigned Folder:**  
**Task / Issue Number:**

#### Planned Work
- [ ] Task 1
- [ ] Task 2
- [ ] Task 3

#### Completed Work
- Describe the changes actually completed.
- Mention the files or modules modified.
- Link the related pull request or commit when available.

#### Files Changed
- `path/to/file`
- `path/to/another-file`

#### Commands and Tests

| Command or Test | Result | Notes |
|---|---|---|
| Command/test name | Passed / Failed / Not run | Actual observation |

#### Blockers
- Describe any error, missing dependency, permission problem, or unresolved question.
- Mention the assistance required.

#### Decisions or Dependencies
- Record decisions needed from the project head or another folder owner.

#### Next Steps
- [ ] Next task 1
- [ ] Next task 2

#### End-of-Day Status

**Status:** Completed / In Progress / Blocked

**Commit or PR:** Add the link if available.

---

## 3. Daily Checklist

### Before Starting Work

- [ ] Check the assigned task.
- [ ] Review relevant documentation.
- [ ] Check `git status`.
- [ ] Synchronize with the latest repository changes.
- [ ] Confirm the folder ownership boundaries.

### Before Ending Work

- [ ] Review changed files.
- [ ] Run relevant checks or tests.
- [ ] Record actual results.
- [ ] Document blockers.
- [ ] Commit only intended changes.
- [ ] Record the next task.

## 4. Example Entry — Illustrative Only

The following example demonstrates the format. It is not a record of actual project progress.

### Date: YYYY-MM-DD

**Contributor:** Example Contributor  
**Assigned Folder:** Example service folder  
**Task / Issue Number:** Example issue

**Planned Work**
- Inspect existing implementation.
- Identify the relevant test command.

**Completed Work**
- Reviewed the assigned module.
- Documented one identified issue.

**Files Changed**
- `example-folder/example-file.md`

**Tests**
- Documentation review: completed.
- Automated tests: not run.

**Blockers**
- Waiting for clarification about an API contract.

**Next Steps**
- Confirm the API contract.
- Continue implementation after approval.

**Status:** In Progress

## 5. Progress Reporting Rules

- Record only work that actually happened.
- Distinguish completed tasks from tasks still in progress.
- Report failed tests honestly.
- Include relevant commit or pull request links.
- Record blockers as soon as they appear.
- Never include credentials or private information in progress logs.

## 6. Weekly Review

At the end of the week, summarize:

- Major completed tasks
- Work still in progress
- Tests and validation completed
- Important blockers
- Dependencies on other contributors
- Priorities for the next week