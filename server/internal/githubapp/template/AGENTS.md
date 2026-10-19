# Agent workflow

All Developer, Reviewer-Security, and QA/DevOps profiles use the pinned environment and the commands in `.multica/environment.json`. Future profiles must use the same image digest, commands, and artifacts.

Order: Developer -> Reviewer-Security -> QA/DevOps. Preview and release are non-production by default.

Dependencies are declared in the language manifest. Secrets are referenced by variable name only and never committed.
