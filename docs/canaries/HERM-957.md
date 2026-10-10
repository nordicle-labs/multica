# HERM-957 database canary

- Executed at: `2026-10-10T13:05:07Z`
- Runtime: `7976d4c7-c5a6-490e-9823-b48cddc7aaa2`
- Host daemon: `f874a5b97762884ac6416a2fff4e92e79f18f5f1`
- Base SHA: `621617421d39c38cdfa58689372df5e4733f286a`

## Results

- `DATABASE_URL`: present; value not printed.
- Provisioned non-local network target: connection succeeded.
- PostgreSQL: `server_version_num=170011` (`17.11`), satisfying `>= 170000`.
- pgvector: version `0.8.7`, available and installed.
- `localhost:5432`: connection refused, confirming no test database is assumed there.

**Canary result: PASS.**
