-- Runs only when Postgres initializes an empty data directory.
-- The sal-pub server applies its own migrations to this database at startup.
CREATE DATABASE salpub;
