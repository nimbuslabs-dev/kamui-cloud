# Kamui Cloud — Demo Script

## 1. Objective

This document provides a repeatable structure for presenting Kamui Cloud.

The demonstration should explain the project's purpose, architecture, development status, and verified functionality.

**Important:** This script is a template. Demonstrate only features that have actually been implemented and tested.

## 2. Suggested Demo Agenda

| Section | Suggested Time |
|---|---:|
| Introduction | 1 minute |
| Problem and motivation | 1 minute |
| Architecture overview | 2 minutes |
| Repository and service structure | 2 minutes |
| Live demonstration | 3–5 minutes |
| Current limitations and next steps | 1 minute |

Adjust the duration to suit the presentation.

## 3. Introduction — What to Say

"Kamui Cloud is a Nimbus Labs project focused on building a unified, intranet-first cloud platform.

The goal is to allow authorized computers connected to a private network to participate in shared computing and service workflows.

Instead of depending on a third-party public cloud for the basic workflow, the project explores how computing resources and services can be managed within a private network."

## 4. Explain the Planned Architecture

Introduce the following components:

- **API:** Central coordination and management.
- **Agent:** Connects a participating node to the coordinator.
- **Store:** File and object storage.
- **Run:** Workload execution.
- **CLI:** Terminal-based management.
- **Train:** Machine-learning training.
- **Infer:** Model inference.
- **Console:** Web dashboard.
- **Drop:** Private-network file sharing.

Explain that each component has a specific responsibility.

Do not claim that all components are functional. Refer to the current repository and tests to determine which services can be demonstrated.

## 5. Live Demonstration Preparation

Before the demo:

- [ ] Confirm the correct branch and repository state.
- [ ] Check that required dependencies are installed.
- [ ] Verify the commands for the service being demonstrated.
- [ ] Start only the services that are supported.
- [ ] Check the relevant health endpoints or test commands.
- [ ] Prepare sample data that contains no secrets.
- [ ] Prepare screenshots or logs as a fallback.
- [ ] Confirm that all demo machines are authorized.

## 6. Suggested Demonstration Sequence

Use this sequence when the corresponding features are implemented.

### Step 1: Show the Repository

Show the project structure and explain the purpose of the main service folders.

### Step 2: Start the Coordinator

Use the startup command documented by the actual API implementation.

Show the real startup output or health check.

### Step 3: Register a Worker

Start the agent on an authorized worker machine using the configured coordinator address.

Show the actual registration result and heartbeat information.

### Step 4: Inspect Nodes

Use the implemented CLI or dashboard to list registered nodes.

Explain the information that is genuinely available.

### Step 5: Submit a Test Job

If the job execution service is implemented, submit a safe test job using the actual documented command.

Show its real status, logs, and result.

### Step 6: Demonstrate Storage

If storage is implemented, upload a small sample file and download it again. Verify the file contents or checksum.

### Step 7: Show the Dashboard

Show actual API-backed node, job, or storage information where available.

Do not use fabricated metrics as live system data.

## 7. If a Feature Is Not Implemented

If a step cannot be demonstrated:

- State that the feature is under development.
- Show the relevant design or code if useful.
- Explain the intended behavior.
- Do not simulate a successful result without clearly labelling it as a mock.
- Continue with the next working part of the demo.

## 8. Closing Statement

"Kamui Cloud aims to bring private-network computing and cloud-style services into one platform. The project is being developed incrementally, beginning with reliable node connectivity and coordination, then extending to workload execution, storage, ML capabilities, and the management interface."

## 9. Post-Demo Checklist

- [ ] Record which features were demonstrated.
- [ ] Note any errors or unexpected behavior.
- [ ] Record questions and feedback.
- [ ] Identify follow-up tasks.
- [ ] Update documentation when verified changes affect the demo.

## 10. Demo Accuracy

A planned feature is not a completed feature. Every claim made during the demonstration should be supported by the current implementation, test results, or clearly labelled design documentation.