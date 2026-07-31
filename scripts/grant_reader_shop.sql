-- Allow the binlog reader to read schema metadata for M0.
-- In production, the unredo_reader should be granted SELECT only on the
-- business schemas it needs to inspect.
GRANT SELECT ON unredo_shop.* TO 'unredo_reader'@'127.0.0.1';
FLUSH PRIVILEGES;
