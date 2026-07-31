-- Allow the binlog reader to see the marker table so the schema
-- inspector can read its columns when the binlog stream mentions it
-- (e.g. an INSERT into action_markers shows up as a row event).
GRANT SELECT ON unredo_meta.* TO 'unredo_reader'@'127.0.0.1';
FLUSH PRIVILEGES;
