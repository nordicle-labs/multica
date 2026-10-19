-- Repair migration is intentionally irreversible because migration 572 may
-- own the same index on fresh databases.
SELECT 1;
