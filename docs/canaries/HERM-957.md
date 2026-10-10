# HERM-957 database canary

- Executed at: `2026-10-10T14:58:28Z`
- Runtime: `7976d4c7-c5a6-490e-9823-b48cddc7aaa2`
- Host daemon: `v0.6.1-22-g134ce4b19`
- Base SHA: `daa0350bd06ea2af6a0bed806c90b08d66295461`
- Worker: `/workspace`, branch `agent/engineering-developer/herm-957`, clean at base SHA.

## Results

- `DATABASE_URL`: present; value not printed.
- Provisioned non-local network target: connection succeeded.
- PostgreSQL: `server_version_num=170011` (`17.11`), satisfying `>= 170000`.
- pgvector: version `0.8.7`, available and installed.
- `localhost:5432`: connection refused, confirming no test database is assumed there.

**Canary result: PASS.**
