-- Repair migration is intentionally irreversible: its objects may predate it
-- on fresh databases, and dropping them would destroy product-intake evidence.
SELECT 1;
